package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"
	"encoding/json"

	"github.com/open-ch/ja3"
	"golang.org/x/net/http2"
)

// Intercept TLS (HTTPS) connections.

// loadCertificate loads the TLS certificate specified by certFile and keyFile
// into tlsCert.
func (c *config) loadCertificate() {
	if c.CertFile != "" && c.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			log.Println("Error loading TLS certificate:", err)
			return
		}
		c.TLSCert = cert
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			log.Println("Error parsing X509 certificate:", err)
			return
		}
		c.ParsedTLSCert = parsed
		c.TLSReady = true

		c.ServeMux.HandleFunc("/cert.der", func(w http.ResponseWriter, r *http.Request) {
			tlsCert := c.TLSCert
			w.Header().Set("Content-Type", "application/x-x509-ca-cert")
			w.Write(tlsCert.Certificate[len(tlsCert.Certificate)-1])
		})
	}
}

// connectDirect connects to serverAddr and copies data between it and conn.
// extraData is sent to the server first.
func connectDirect(conn net.Conn, serverAddr string, extraData []byte) (uploaded, downloaded int64) {
	activeConnections.Add(1)
	defer activeConnections.Done()

	serverConn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		log.Printf("error with pass-through of SSL connection to %s: %s", serverAddr, err)
		conn.Close()
		return
	}

	if extraData != nil {
		// There may also be data waiting in the socket's input buffer;
		// read it before we send the data on, so that the first packet of
		// the connection doesn't get split in two.
		conn.SetReadDeadline(time.Now().Add(time.Millisecond))
		buf := make([]byte, 2000)
		n, _ := conn.Read(buf)
		conn.SetReadDeadline(time.Time{})
		if n > 0 {
			extraData = append(extraData, buf[:n]...)
		}
		serverConn.Write(extraData)
	}

	ulChan := make(chan int64)
	go func() {
		n, _ := io.Copy(conn, serverConn)
		time.Sleep(time.Second)
		conn.Close()
		ulChan <- n + int64(len(extraData))
	}()
	downloaded, _ = io.Copy(serverConn, conn)
	serverConn.Close()
	uploaded = <-ulChan
	return uploaded, downloaded
}

type tlsFingerprintKey struct{}

// SSLBump performs a man-in-the-middle attack on conn, to filter the HTTPS
// traffic. serverAddr is the address (host:port) of the server the client was
// trying to connect to. user is the username to use for logging; authUser is
// the authenticated user, if any; r is the CONNECT request, if any.
func SSLBump(conn net.Conn, serverAddr, user, authUser string, r *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			buf := make([]byte, 4096)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("SSLBump: panic serving connection to %s: %v\n%s", serverAddr, err, buf)
			conn.Close()
		}
	}()

	conf := getConfig()

	obsoleteVersion := false
	invalidSSL := false
	// Read the client hello so that we can find out the name of the server (not
	// just the address).
	clientHello, err := readClientHello(conn)
	if err != nil {
		logTLS(user, serverAddr, "", fmt.Errorf("error reading client hello: %v", err), false, "")
		if _, ok := err.(net.Error); ok {
			conn.Close()
			return
		} else if err == ErrObsoleteSSLVersion {
			obsoleteVersion = true
			if conf.BlockObsoleteSSL {
				conn.Close()
				return
			}
		} else if err == ErrInvalidSSL {
			invalidSSL = true
		} else {
			conn.Close()
			return
		}
	}

	host, port, err := net.SplitHostPort(serverAddr)
	if err != nil {
		host = serverAddr
		port = "443"
	}

	serverName := ""
	if !obsoleteVersion && !invalidSSL {
		if sn, ok := clientHelloServerName(clientHello); ok && sn != "" {
			serverName = sn
			serverAddr = net.JoinHostPort(sn, port)
		}
	}
	sni := serverName

	if serverName == "" {
		serverName = host
		if ip := net.ParseIP(serverName); ip != nil {
			// All we have is an IP address, not a name from a CONNECT request.
			// See if we can do better by reverse DNS.
			names, err := net.LookupAddr(serverName)
			if err == nil && len(names) > 0 {
				serverName = strings.TrimSuffix(names[0], ".")
			}
		}
	}

	// Filter a virtual CONNECT request.
	cr := &http.Request{
		Method:     "CONNECT",
		Header:     make(http.Header),
		Host:       net.JoinHostPort(serverName, port),
		URL:        &url.URL{Host: serverName},
		RemoteAddr: conn.RemoteAddr().String(),
	}

	var tlsFingerprint string
	j, err := ja3.ComputeJA3FromSegment(clientHello)
	if err != nil {
		log.Printf("Error generating TLS fingerprint: %v", err)
	} else {
		tlsFingerprint = j.GetJA3Hash()
		ctx := cr.Context()
		ctx = context.WithValue(ctx, tlsFingerprintKey{}, tlsFingerprint)
		cr = cr.WithContext(ctx)
	}

	tally := conf.URLRules.MatchingRules(cr.URL)
	scores := conf.categoryScores(tally)

	reqACLs := conf.ACLs.requestACLs(cr, authUser)
	if invalidSSL {
		reqACLs["invalid-ssl"] = true
	}

	possibleActions := []string{
		"allow",
		"block",
	}
	if conf.TLSReady && !obsoleteVersion && !invalidSSL {
		possibleActions = append(possibleActions, "ssl-bump")
	}

	rule, ignored := conf.ChooseACLCategoryAction(reqACLs, scores, conf.Threshold, possibleActions...)

	for _, externalACL := range conf.ExternalConnectACL {
			v := make(url.Values)
			v.Set("url", cr.URL.String())
			v.Set("method", cr.Method)
			v.Set("action", rule.Action)
			v.Set("src", cr.RemoteAddr)


			localCr, err := clientWithExtraRootCerts.PostForm(externalACL, v)
			if err != nil {
				log.Printf("Error checking external-connect-acl (%s): %v", externalACL, err)
				continue
			}
			if localCr.StatusCode != 200 {
				log.Printf("Bad HTTP status checking external-connect-acl (%s): %s", externalACL, localCr.Status)
				continue
			}
			jd := json.NewDecoder(localCr.Body)
			externalAclsAction := make(map[string]int)
			err = jd.Decode(&externalAclsAction)
			localCr.Body.Close()
			if err != nil {
				log.Printf("Error decoding response from external-connect-acl (%s): %v", externalACL, err)
				continue
			}
			for k := range externalAclsAction {
				if k == "ssl-bump" || k == "tlsbump" {
					if conf.TLSReady && !obsoleteVersion && !invalidSSL {
						rule.Action = "ssl-bump"
					}
				}
				if k == "allow" || k == "bumpbypass"{
					rule.Action = "allow"
				}
				if k == "block" {
					rule.Action = k
				}
			}
	}


	logAccess(cr, nil, 0, false, user, tally, scores, rule, "", ignored)


	switch rule.Action {
	case "allow", "":
		conf = nil
		upload, download := connectDirect(conn, serverAddr, clientHello)
		logAccess(cr, nil, int(upload+download), false, user, tally, scores, rule, "", ignored)
		return
	case "block":
		conn.Close()
		return
	}

	cert, rt, http2Support := conf.CertCache.Get(serverName, serverAddr)
	cachedCert := rt != nil
	if !cachedCert {
		serverConn, err := tls.DialWithDialer(dialer, "tcp", serverAddr, &tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2", "http/1.1"},
		})
		if err == nil {
			state := serverConn.ConnectionState()
			serverConn.Close()
			serverCert := state.PeerCertificates[0]

			valid := conf.validCert(serverCert, state.PeerCertificates[1:])
			cert, err = imitateCertificate(serverCert, !valid, conf, sni)
			if err != nil {
				serverConn.Close()
				logTLS(user, serverAddr, serverName, fmt.Errorf("error generating certificate: %v", err), cachedCert, tlsFingerprint)
				conf = nil
				connectDirect(conn, serverAddr, clientHello)
				return
			}

			_, err = serverCert.Verify(x509.VerifyOptions{
				Intermediates: certPoolWith(state.PeerCertificates[1:]),
				DNSName:       serverName,
			})
			validWithDefaultRoots := err == nil

			if validWithDefaultRoots {
				rt = httpTransport
			} else {
				rt = newHardValidationTransport(insecureHTTPTransport, serverName, state.PeerCertificates)
			}
			http2Support = state.NegotiatedProtocol == "h2" && state.NegotiatedProtocolIsMutual
		} else {
			cert, err = fakeCertificate(conf, sni)
			if err != nil {
				logTLS(user, serverAddr, serverName, fmt.Errorf("error generating certificate: %v", err), cachedCert, tlsFingerprint)
				conn.Close()
				return
			}
			rt = httpTransport
		}
		conf.CertCache.Put(serverName, serverAddr, cert, rt, http2Support)
	}

	server := http.Server{
		Handler: proxyHandler{
			TLS:            true,
			tlsFingerprint: tlsFingerprint,
			connectPort:    port,
			user:           authUser,
			rt:             rt,
		},
		TLSConfig: &tls.Config{
			Certificates:             []tls.Certificate{cert, conf.TLSCert},
			PreferServerCipherSuites: true,
			CurvePreferences: []tls.CurveID{
				tls.CurveP256,
				tls.X25519, // Go 1.8 only
			},
		},
		IdleTimeout: conf.CloseIdleConnections,
	}

	if conf.HTTP2Downstream && http2Support {
		server.TLSConfig.NextProtos = []string{"h2", "http/1.1"}
		err = http2.ConfigureServer(&server, nil)
		if err != nil {
			log.Println("Error configuring HTTP/2 server:", err)
		}
	}

	tlsConn := tls.Server(&insertingConn{conn, clientHello}, server.TLSConfig)
	err = tlsConn.Handshake()
	if err != nil {
		logTLS(user, serverAddr, serverName, fmt.Errorf("error in handshake with client: %v", err), cachedCert, tlsFingerprint)
		conn.Close()
		return
	}

	listener := &singleListener{conn: tlsConn}
	logTLS(user, serverAddr, serverName, nil, cachedCert, tlsFingerprint)
	conf = nil
	server.Serve(listener)
}

func certPoolWith(certs []*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	return pool
}

// A insertingConn is a net.Conn that inserts extra data at the start of the
// incoming data stream.
type insertingConn struct {
	net.Conn
	extraData []byte
}

func (c *insertingConn) Read(p []byte) (n int, err error) {
	if len(c.extraData) == 0 {
		return c.Conn.Read(p)
	}

	n = copy(p, c.extraData)
	c.extraData = c.extraData[n:]
	return
}

// A singleListener is a net.Listener that returns a single connection, then
// gives the error io.EOF.
type singleListener struct {
	conn net.Conn
	once sync.Once
}

func (s *singleListener) Accept() (net.Conn, error) {
	var c net.Conn
	s.once.Do(func() {
		c = s.conn
	})
	if c != nil {
		return c, nil
	}
	return nil, io.EOF
}

func (s *singleListener) Close() error {
	s.once.Do(func() {
		s.conn.Close()
	})
	return nil
}

func (s *singleListener) Addr() net.Addr {
	return s.conn.LocalAddr()
}

// imitateCertificate returns a new TLS certificate that has most of the same
// data as serverCert but is signed by Redwood's root certificate, or
// self-signed.
func imitateCertificate(serverCert *x509.Certificate, selfSigned bool, conf *config, sni string) (cert tls.Certificate, err error) {
	// Use a hash of the real certificate as the serial number.
	h := md5.New()
	h.Write(serverCert.Raw)
	h.Write([]byte{2})
	if sni != "" {
		io.WriteString(h, sni)
	}

	template := &x509.Certificate{
		SerialNumber:                big.NewInt(0).SetBytes(h.Sum(nil)),
		Subject:                     serverCert.Subject,
		NotBefore:                   serverCert.NotBefore,
		NotAfter:                    serverCert.NotAfter,
		KeyUsage:                    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:                 serverCert.ExtKeyUsage,
		UnknownExtKeyUsage:          serverCert.UnknownExtKeyUsage,
		BasicConstraintsValid:       false,
		SubjectKeyId:                nil,
		DNSNames:                    serverCert.DNSNames,
		PermittedDNSDomainsCritical: serverCert.PermittedDNSDomainsCritical,
		PermittedDNSDomains:         serverCert.PermittedDNSDomains,
		SignatureAlgorithm:          x509.UnknownSignatureAlgorithm,
	}

	// If sni is not blank, make a certificate that covers only that domain,
	// instead of all the domains covered by the original certificate.
	if sni != "" {
		template.DNSNames = []string{sni}
		template.Subject.CommonName = sni
	}

	var newCertBytes []byte
	if selfSigned {
		newCertBytes, err = x509.CreateCertificate(rand.Reader, template, template, conf.ParsedTLSCert.PublicKey, conf.TLSCert.PrivateKey)
	} else {
		newCertBytes, err = x509.CreateCertificate(rand.Reader, template, conf.ParsedTLSCert, conf.ParsedTLSCert.PublicKey, conf.TLSCert.PrivateKey)
	}
	if err != nil {
		return tls.Certificate{}, err
	}

	newCert := tls.Certificate{
		Certificate: [][]byte{newCertBytes},
		PrivateKey:  conf.TLSCert.PrivateKey,
	}

	if !selfSigned {
		newCert.Certificate = append(newCert.Certificate, conf.TLSCert.Certificate...)
	}
	return newCert, nil
}

// fakeCertificate returns a fabricated certificate for the server identified by sni.
func fakeCertificate(conf *config, sni string) (cert tls.Certificate, err error) {
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, err
	}
	y, m, d := time.Now().Date()

	template := &x509.Certificate{
		SerialNumber:       serial,
		Subject:            pkix.Name{CommonName: sni},
		NotBefore:          time.Date(y, m, d, 0, 0, 0, 0, time.Local),
		NotAfter:           time.Date(y, m+1, d, 0, 0, 0, 0, time.Local),
		KeyUsage:           x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		DNSNames:           []string{sni},
		SignatureAlgorithm: x509.UnknownSignatureAlgorithm,
	}

	newCertBytes, err := x509.CreateCertificate(rand.Reader, template, conf.ParsedTLSCert, conf.ParsedTLSCert.PublicKey, conf.TLSCert.PrivateKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	newCert := tls.Certificate{
		Certificate: [][]byte{newCertBytes},
		PrivateKey:  conf.TLSCert.PrivateKey,
	}

	newCert.Certificate = append(newCert.Certificate, conf.TLSCert.Certificate...)
	return newCert, nil
}

func (conf *config) validCert(cert *x509.Certificate, intermediates []*x509.Certificate) bool {
	pool := certPoolWith(intermediates)
	_, err := cert.Verify(x509.VerifyOptions{Intermediates: pool})
	if err == nil {
		return true
	}
	if _, ok := err.(x509.UnknownAuthorityError); !ok {
		// There was an error, but not because the certificate wasn't signed
		// by a recognized CA. So we go ahead and use the cert and let
		// the client experience the same error.
		return true
	}

	if conf.ExtraRootCerts != nil {
		_, err = cert.Verify(x509.VerifyOptions{Roots: conf.ExtraRootCerts, Intermediates: pool})
		if err == nil {
			return true
		}
		if _, ok := err.(x509.UnknownAuthorityError); !ok {
			return true
		}
	}

	// Before we give up, we'll try fetching some intermediate certificates.
	if len(cert.IssuingCertificateURL) == 0 {
		return false
	}

	toFetch := cert.IssuingCertificateURL
	fetched := make(map[string]bool)

	for i := 0; i < len(toFetch); i++ {
		certURL := toFetch[i]
		if fetched[certURL] {
			continue
		}
		resp, err := http.Get(certURL)
		if err == nil {
			defer resp.Body.Close()
		}
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		fetchedCert, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			continue
		}

		// The fetched certificate might be in either DER or PEM format.
		if bytes.Contains(fetchedCert, []byte("-----BEGIN CERTIFICATE-----")) {
			// It's PEM.
			var certDER *pem.Block
			for {
				certDER, fetchedCert = pem.Decode(fetchedCert)
				if certDER == nil {
					break
				}
				if certDER.Type != "CERTIFICATE" {
					continue
				}
				thisCert, err := x509.ParseCertificate(certDER.Bytes)
				if err != nil {
					continue
				}
				pool.AddCert(thisCert)
				toFetch = append(toFetch, thisCert.IssuingCertificateURL...)
			}
		} else {
			// Hopefully it's DER.
			thisCert, err := x509.ParseCertificate(fetchedCert)
			if err != nil {
				continue
			}
			pool.AddCert(thisCert)
			toFetch = append(toFetch, thisCert.IssuingCertificateURL...)
		}
	}

	_, err = cert.Verify(x509.VerifyOptions{Intermediates: pool})
	if err == nil {
		return true
	}
	if _, ok := err.(x509.UnknownAuthorityError); !ok {
		// There was an error, but not because the certificate wasn't signed
		// by a recognized CA. So we go ahead and use the cert and let
		// the client experience the same error.
		return true
	}
	return false
}

var ErrObsoleteSSLVersion = errors.New("obsolete SSL protocol version")
var ErrInvalidSSL = errors.New("invalid first byte for SSL connection; possibly some other protocol")

func readClientHello(conn net.Conn) (hello []byte, err error) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	var header [5]byte
	n, err := io.ReadFull(conn, header[:])
	hello = header[:n]
	if err != nil {
		return hello, err
	}

	if header[0] != 22 {
		if header[0] == 128 {
			return hello, ErrObsoleteSSLVersion
		}
		return hello, ErrInvalidSSL
	}
	if header[1] != 3 {
		return hello, fmt.Errorf("expected major version of 3, got %d", header[1])
	}
	recordLen := int(header[3])<<8 | int(header[4])
	if recordLen > 0x3000 {
		return hello, fmt.Errorf("expected length less than 12kB, got %d", recordLen)
	}
	if recordLen < 4 {
		return hello, fmt.Errorf("expected length of at least 4 bytes, got %d", recordLen)
	}

	protocolData := make([]byte, recordLen)
	n, err = io.ReadFull(conn, protocolData)
	hello = append(hello, protocolData[:n]...)
	if err != nil {
		return hello, err
	}
	if protocolData[0] != 1 {
		return hello, fmt.Errorf("Expected message type 1 (ClientHello), got %d", protocolData[0])
	}
	protocolLen := int(protocolData[1])<<16 | int(protocolData[2])<<8 | int(protocolData[3])
	if protocolLen != recordLen-4 {
		return hello, fmt.Errorf("recordLen=%d, protocolLen=%d", recordLen, protocolLen)
	}

	return hello, nil
}

func clientHelloServerName(data []byte) (name string, ok bool) {
	if len(data) < 5 {
		return "", false
	}
	// Strip off the record header.
	data = data[5:]

	if len(data) < 42 {
		return "", false
	}

	sessionIdLen := int(data[38])
	if sessionIdLen > 32 || len(data) < 39+sessionIdLen {
		return "", false
	}
	data = data[39+sessionIdLen:]
	if len(data) < 2 {
		return "", false
	}

	cipherSuiteLen := int(data[0])<<8 | int(data[1])
	if cipherSuiteLen%2 == 1 || len(data) < 2+cipherSuiteLen {
		return "", false
	}
	data = data[2+cipherSuiteLen:]
	if len(data) < 1 {
		return "", false
	}

	compressionMethodsLen := int(data[0])
	if len(data) < 1+compressionMethodsLen {
		return "", false
	}
	data = data[1+compressionMethodsLen:]
	if len(data) < 2 {
		return "", false
	}

	extensionsLength := int(data[0])<<8 | int(data[1])
	data = data[2:]
	if extensionsLength != len(data) {
		return "", false
	}

	for len(data) != 0 {
		if len(data) < 4 {
			return "", false
		}
		extension := uint16(data[0])<<8 | uint16(data[1])
		length := int(data[2])<<8 | int(data[3])
		data = data[4:]
		if len(data) < length {
			return "", false
		}

		if extension == 0 /* server name */ {
			if length < 2 {
				return "", false
			}
			numNames := int(data[0])<<8 | int(data[1])
			d := data[2:]
			for i := 0; i < numNames; i++ {
				if len(d) < 3 {
					return "", false
				}
				nameType := d[0]
				nameLen := int(d[1])<<8 | int(d[2])
				d = d[3:]
				if len(d) < nameLen {
					return "", false
				}
				if nameType == 0 {
					return string(d[:nameLen]), true
				}
				d = d[nameLen:]
			}
		}

		data = data[length:]
	}

	return "", true
}

func (c *config) addTrustedRoots(certPath string) error {
	if c.ExtraRootCerts == nil {
		c.ExtraRootCerts = x509.NewCertPool()
	}

	pem, err := ioutil.ReadFile(certPath)
	if err != nil {
		return err
	}

	if !c.ExtraRootCerts.AppendCertsFromPEM(pem) {
		return fmt.Errorf("no certificates found in %s", certPath)
	}
	return nil
}

type CertificateCache struct {
	lock        sync.RWMutex
	cache       map[certCacheKey]certCacheEntry
	TTL         time.Duration
	lastCleaned time.Time
}

type certCacheKey struct {
	name, addr string
}

type certCacheEntry struct {
	certificate tls.Certificate
	transport   http.RoundTripper
	http2       bool
	added       time.Time
}

func (c *CertificateCache) Put(serverName, serverAddr string, cert tls.Certificate, transport http.RoundTripper, http2Support bool) {
	c.lock.Lock()
	defer c.lock.Unlock()

	now := time.Now()
	if c.cache == nil {
		c.cache = make(map[certCacheKey]certCacheEntry)
		c.lastCleaned = now
	}

	if now.Sub(c.lastCleaned) > c.TTL {
		// Remove expired entries.
		for k, v := range c.cache {
			if now.Sub(v.added) > c.TTL {
				delete(c.cache, k)
			}
		}
	}

	c.cache[certCacheKey{
		name: serverName,
		addr: serverAddr,
	}] = certCacheEntry{
		certificate: cert,
		transport:   transport,
		http2:       http2Support,
		added:       now,
	}
}

func (c *CertificateCache) Get(serverName, serverAddr string) (tls.Certificate, http.RoundTripper, bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	v, ok := c.cache[certCacheKey{
		name: serverName,
		addr: serverAddr,
	}]

	if !ok || time.Now().Sub(v.added) > c.TTL {
		return tls.Certificate{}, nil, false
	}

	return v.certificate, v.transport, v.http2
}
