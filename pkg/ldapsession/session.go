package ldapsession

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/proxy"

	"github.com/go-ldap/ldap/v3"
	"github.com/ropnop/go-windapsearch/pkg/dns"
	"github.com/sirupsen/logrus"
)

type LDAPSessionOptions struct {
	Domain           string
	DomainController string
	Username         string
	Password         string
	Hash             string
	UseNTLM          bool
	Port             int
	Secure           bool
	Proxy            string
	PageSize         int
	Logger           *logrus.Logger
}

type LDAPSession struct {
	LConn       *ldap.Conn
	PageSize    uint32
	BaseDN      string
	DomainInfo  DomainInfo
	Log         *logrus.Entry
	resultsChan chan *ldap.Entry
	ctx         context.Context
	Channels    *ResultChannels
}

type ResultChannels struct {
	Entries   chan *ldap.Entry
	Referrals chan string
	Controls  chan ldap.Control
	keepOpen  bool
}

type DomainInfo struct {
	Metadata                           *ldap.SearchResult
	DomainFunctionalityLevel           string
	ForestFunctionalityLevel           string
	DomainControllerFunctionalityLevel string
	ServerDNSName                      string
}

func NewLDAPSession(options *LDAPSessionOptions, ctx context.Context) (sess *LDAPSession, err error) {
	logger := logrus.New()
	if options.Logger != nil {
		logger = options.Logger
	}
	sess = &LDAPSession{Log: logger.WithFields(logrus.Fields{"package": "ldapsession"})}

	port := options.Port
	dc := options.DomainController
	if port == 0 {
		if options.Secure {
			port = 636
		} else {
			port = 389
		}
	}
	if dc == "" {
		dcs, err := dns.FindLDAPServers(options.Domain)
		if err != nil {
			return sess, err
		}
		dc = dcs[0]
		sess.Log.Infof("Found LDAP server via DNS: %s", dc)
	}
	var url string

	if options.Secure {
		url = fmt.Sprintf("ldaps://%s:%d", dc, port)
	} else {
		url = fmt.Sprintf("ldap://%s:%d", dc, port)
	}

	var conn net.Conn
	defaultDailer := &net.Dialer{Timeout: ldap.DefaultTimeout}

	// Use socks proxy if specified
	if options.Proxy != "" {
		pDialer, err := proxy.SOCKS5("tcp", options.Proxy, nil, defaultDailer)
		if err != nil {
			return nil, err
		}
		conn, err = pDialer.Dial("tcp", fmt.Sprintf("%s:%d", dc, port))
		if err != nil {
			return nil, err
		}
		sess.Log.Debugf("establishing connection through socks proxy at %s", options.Proxy)
	} else {
		conn, err = defaultDailer.Dial("tcp", fmt.Sprintf("%s:%d", dc, port))
		if err != nil {
			return
		}
	}
	sess.Log.Debugf("tcp connection established to %s:%d", dc, port)

	var lConn *ldap.Conn
	if options.Secure {
		tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
		lConn = ldap.NewConn(tlsConn, options.Secure)
		sess.Log.Debug("TLS connection established")
	} else {
		lConn = ldap.NewConn(conn, options.Secure)
	}

	lConn.Start()

	sess.LConn = lConn
	sess.PageSize = uint32(options.PageSize)

	if options.UseNTLM || options.Hash != "" {
		err = sess.NTLMBind(options.Username, options.Password, options.Hash)
	} else {
		err = sess.SimpleBind(options.Username, options.Password)
	}

	if err != nil {
		return
	}
	sess.Log.Infof("successful bind to %q as %q", url, options.Username)
	_, err = sess.GetDefaultNamingContext()
	if err != nil {
		return
	}
	sess.Log.Infof("retrieved default naming context: %q", sess.BaseDN)

	sess.NewChannels(ctx)
	return sess, nil
}

func (w *LDAPSession) SetChannels(chs *ResultChannels, ctx context.Context) {
	w.Channels = chs
	w.ctx = ctx
}

func (w *LDAPSession) NewChannels(ctx context.Context) {
	w.Log.Debugf("creating new ldapsession channels")
	w.Channels = &ResultChannels{
		Entries:   make(chan *ldap.Entry),
		Referrals: make(chan string),
		Controls:  make(chan ldap.Control),
		keepOpen:  false,
	}
	w.ctx = ctx
}

// If you call this, the results channels will not automatically close when a search is finished and
// will need to be manually closed with CloseChannels(). Be careful here - this can
// cause all sorts of concurrency race conditions
func (w *LDAPSession) keepChannelsOpen() {
	w.Channels.keepOpen = true
}

func (w *LDAPSession) CloseChannels() {
	if w.Channels.Entries != nil {
		close(w.Channels.Entries)
	}
	if w.Channels.Controls != nil {
		close(w.Channels.Controls)
	}
	if w.Channels.Referrals != nil {
		close(w.Channels.Referrals)
	}
	w.Log.Debugf("closing ldapsession channels")

}

//func (w *LDAPSession) SetResultsChannel(ch chan *ldap.Entry, ctx context.Context) {
//	w.resultsChan = ch
//	w.ctx = ctx
//}

func (w *LDAPSession) SimpleBind(username, password string) (err error) {
	if username == "" {
		err = w.LConn.UnauthenticatedBind("")
	} else {
		err = w.LConn.Bind(username, password)
	}
	if err != nil {
		return
	}
	return
}

func (w *LDAPSession) NTLMBind(username, password, hash string) (err error) {
	userParts := strings.Split(username, "@")
	user := userParts[0]
	domain := strings.Join(userParts[1:], "")

	if hash != "" {
		w.Log.Infof("attempting PtH NTLM bind for %q", user)
		return w.LConn.NTLMBindWithHash(domain, user, hash)
	}
	w.Log.Infof("attempting NTLM bind for %q", user)
	return w.LConn.NTLMBind(domain, user, password)
}

func (w *LDAPSession) Close() {
	w.LConn.Close()
}

func (w *LDAPSession) GetDefaultNamingContext() (string, error) {
	if w.BaseDN != "" {
		return w.BaseDN, nil
	}
	sr := ldap.NewSearchRequest(
		"",
		ldap.ScopeBaseObject,
		ldap.NeverDerefAliases,
		0, 0, false,
		"(objectClass=*)",
		[]string{"defaultNamingContext"},
		nil)
	res, err := w.LConn.Search(sr)
	if err != nil {
		return "", err
	}
	if len(res.Entries) == 0 {
		return "", fmt.Errorf("error getting metadata: No LDAP responses from server")
	}
	defaultNamingContext := res.Entries[0].GetAttributeValue("defaultNamingContext")
	if defaultNamingContext == "" {
		return "", fmt.Errorf("error getting metadata: attribute defaultNamingContext missing")
	}
	w.BaseDN = defaultNamingContext
	return w.BaseDN, nil

}

func (w *LDAPSession) ReturnMetadataResults() error {
	for _, entry := range w.DomainInfo.Metadata.Entries {
		w.resultsChan <- entry
	}
	return nil
}
