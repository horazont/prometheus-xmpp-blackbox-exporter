package prober

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"mellium.im/xmlstream"
	"mellium.im/xmpp"
	"mellium.im/xmpp/jid"
	"mellium.im/xmpp/stanza"

	"github.com/horazont/prometheus-xmpp-blackbox-exporter/internal/config"
)

type AccountInfo struct {
	Registered bool
	Account    jid.JID
	Password   string
}

type ibrCheckContext struct {
	ctx         context.Context
	config      *config.IBRProbe
	streamFrom  jid.JID
	domain      jid.JID
	accountInfo AccountInfo
	tlsConfig   *tls.Config

	durationGaugeVec *prometheus.GaugeVec
}

func (c *ibrCheckContext) executeRegistration(conn net.Conn, tls_config *tls.Config, ct *connTrace) (tls_state *tls.ConnectionState, err error) {
	capture := NewCapturingStartTLS(tls_config)

	features := make([]xmpp.StreamFeature, 0)
	if tls_config != nil {
		features = append(features, traceStreamFeature(capture.ToStreamFeature(), &ct.starttlsDone))
	}
	features = append(features, Register(c.config.Prefix, c.domain.Domainpart(), &c.accountInfo.Account, &c.accountInfo.Password))

	session, err := xmpp.NegotiateSession(
		c.ctx,
		c.domain,
		c.streamFrom,
		conn,
		false,
		xmpp.NewNegotiator(
			xmpp.StreamConfig{
				Lang:     "en",
				Features: features,
			},
		),
	)
	defer session.Close()

	if err != nil {
		return tls_state, err
	}

	c.accountInfo.Registered = true
	ct.authDone = time.Now()

	if tls_config != nil {
		tls_conn := capture.CapturedWriter.(*tls.Conn)
		err = tls_conn.Handshake()
		if err != nil {
			return tls_state, err
		}

		tls_state = &tls.ConnectionState{}
		*tls_state = tls_conn.ConnectionState()
	}

	return tls_state, nil
}

func (c *ibrCheckContext) loginAndDeregister(ct *connTrace) error {
	ct_tmp, conn, session, err := login(
		c.ctx,
		c.tlsConfig,
		c.accountInfo.Account,
		c.accountInfo.Password,
		c.config.DirectTLS,
	)
	*ct = ct_tmp
	if err != nil {
		return fmt.Errorf("failed to establish session for %s: %s", c.accountInfo.Account, err)
	}
	defer conn.Close()
	defer session.Close()

	return nil
}

func cancelRegistrationTokenReader() xml.TokenReader {
	return xmlstream.Wrap(
		xmlstream.MultiReader(
			xmlstream.Wrap(
				nil,
				xml.StartElement{Name: xml.Name{
					Local: "remove",
				}},
			),
		),
		xml.StartElement{Name: xml.Name{
			Space: "jabber:iq:register",
			Local: "query",
		}},
	)
}

func ProbeIBR(ctx context.Context, target string, config config.Module, registry *prometheus.Registry) bool {
	host, addr, err := parseTarget(target, false)
	if err != nil {
		log.Printf("failed to parse target %s: %s", target, err)
		return false
	}

	tls_config, err := newTLSConfig(&config.IBR.TLSConfig, addr.Domainpart())
	if err != nil {
		log.Printf("failed to process TLS config: %s", err)
		return false
	}

	c := &ibrCheckContext{
		ctx:        ctx,
		config:     &config.IBR,
		streamFrom: addr,
		domain:     addr.Domain(),
		tlsConfig:  tls_config,
		durationGaugeVec: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_xmpp_duration_seconds",
			Help: "Duration of xmpp connection by phase",
		}, []string{"phase"}),
	}

	registry.MustRegister(c.durationGaugeVec)

	ct := connTrace{}
	ct.auth = false
	ct.starttls = !config.IBR.DirectTLS
	ct.start = time.Now()

	_, conn, err := dialXMPP(ctx, config.IBR.DirectTLS, tls_config, host, addr, false)
	if err != nil {
		log.Printf("failed to probe c2s to %s: %s", target, err)
		return false
	}
	defer conn.Close()

	ct.connectDone = time.Now()
	c.durationGaugeVec.WithLabelValues("connect").Set(ct.connectDone.Sub(ct.start).Seconds())

	{
		tls_config_to_pass := tls_config
		if config.C2S.DirectTLS {
			tls_config_to_pass = nil
		}
		_, err = c.executeRegistration(conn, tls_config_to_pass, &ct)
	}

	if !ct.starttls {
		ct.starttlsDone = ct.connectDone
	}
	c.durationGaugeVec.WithLabelValues("starttls").Set(ct.starttlsDone.Sub(ct.connectDone).Seconds())
	c.durationGaugeVec.WithLabelValues("register").Set(ct.authDone.Sub(ct.starttlsDone).Seconds())

	if err != nil {
		log.Printf("registration failed: %s", err.Error())
		return false
	}

	ct, conn, session, err := login(
		c.ctx,
		c.tlsConfig,
		c.accountInfo.Account,
		c.accountInfo.Password,
		c.config.DirectTLS,
	)
	if err != nil {
		log.Printf("failed to establish session for %s: %s", c.accountInfo.Account, err)
	}
	defer conn.Close()
	defer session.Close()

	c.durationGaugeVec.WithLabelValues("cancel-connect").Set(ct.connectDone.Sub(ct.start).Seconds())
	c.durationGaugeVec.WithLabelValues("cancel-starttls").Set(ct.starttlsDone.Sub(ct.connectDone).Seconds())
	c.durationGaugeVec.WithLabelValues("cancel-auth").Set(ct.authDone.Sub(ct.starttlsDone).Seconds())

	go session.Serve(nil)

	response_stream, err := session.SendIQ(ctx, stanza.IQ{
		Type: stanza.SetIQ,
	}.Wrap(cancelRegistrationTokenReader()))
	if response_stream != nil {
		defer response_stream.Close()
	}

	if err != nil {
		log.Printf("failed to cancel registration: %s", err.Error())
		return false
	}

	response := struct {
		stanza.IQ
		Error stanza.Error `xml:"jabber:client error"`
	}{}
	d := xml.NewTokenDecoder(response_stream)
	start_token, err := d.Token()
	start := start_token.(xml.StartElement)
	err = d.DecodeElement(&response, &start)
	if err != nil {
		log.Printf("failed to decode cancellation reply: %s", err.Error())
		return false
	}

	if response.Type != stanza.ResultIQ {
		log.Printf("failed to cancel registration: %s", response.Error.Condition)
		return false
	}

	return true
}