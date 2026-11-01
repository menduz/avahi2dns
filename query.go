package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/godbus/dbus/v5"
	"github.com/holoplot/go-avahi"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

// TTL of the A and AAAA answers (the default of dns.NewRR).
const answerTTL = 3600

// D-Bus errors with which avahi says that the host does not exist (or cannot
// exist), as opposed to a failure of avahi itself.
var avahiMissErrors = map[string]bool{
	"org.freedesktop.Avahi.TimeoutError":         true,
	"org.freedesktop.Avahi.NotFoundError":        true,
	"org.freedesktop.Avahi.InvalidHostNameError": true,
}

type family struct {
	label string
	proto int32
	qtype uint16
}

var (
	familyV4 = family{label: "A", proto: avahi.ProtoInet, qtype: dns.TypeA}
	familyV6 = family{label: "AAAA", proto: avahi.ProtoInet6, qtype: dns.TypeAAAA}
)

// lookupResult is the outcome of the lookup of one name in one address family.
type lookupResult struct {
	family family
	// exists is true when avahi found the host in this family.
	exists bool
	// address is valid when exists is true and the address is usable.
	address netip.Addr
	// err is a failure of avahi (not a missing host).
	err error
}

func (me *forwarder) onDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	me.logger.WithField("request", r).Debug("received request")
	m := me.forwardToAvahi(r)
	me.logger.WithField("reply", m).Debug("sending reply")
	err := w.WriteMsg(m)
	if err != nil {
		me.logger.WithError(err).Error("failed to write response")
	}
}

func (me *forwarder) forwardToAvahi(r *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Compress = false
	m.Authoritative = true

	// The server only accepts queries with one question (see
	// dns.DefaultMsgAcceptFunc).
	q := r.Question[0]
	zone := me.zoneOf(q.Name)

	if strings.EqualFold(q.Name, zone) {
		// The apex has the SOA record and nothing else.
		if q.Qtype == dns.TypeSOA {
			m.Answer = append(m.Answer, me.soaRecord(zone))
		} else {
			m.Ns = append(m.Ns, me.soaRecord(zone))
		}
		return m
	}

	rcode, answer := me.resolve(q)
	m.Rcode = rcode
	switch {
	case answer != nil:
		m.Answer = append(m.Answer, answer)
	case rcode == dns.RcodeSuccess || rcode == dns.RcodeNameError:
		// Negative answer (NODATA or NXDOMAIN): the SOA gives its TTL.
		m.Ns = append(m.Ns, me.soaRecord(zone))
	}
	return m
}

// resolve looks up the name of q in each enabled address family, in parallel.
// It returns as soon as the family of q has an address. Otherwise it waits for
// all the lookups:
//   - the host exists in a family: NOERROR without an answer (NODATA)
//   - a lookup failed: SERVFAIL
//   - else: NXDOMAIN
func (me *forwarder) resolve(q dns.Question) (int, dns.RR) {
	families := make([]family, 0, 2)
	if !me.v6only {
		families = append(families, familyV4)
	}
	if !me.v4only {
		families = append(families, familyV6)
	}

	results := make(chan lookupResult, len(families))
	for _, f := range families {
		go func() {
			results <- me.lookup(q.Name, f)
		}()
	}

	exists := false
	failed := false
	for range families {
		res := <-results
		if res.err != nil {
			failed = true
			continue
		}
		if !res.exists {
			continue
		}
		if res.family.qtype == q.Qtype && res.address.IsValid() {
			return dns.RcodeSuccess, addressRecord(q.Name, res)
		}
		if q.Qtype != dns.TypeA && q.Qtype != dns.TypeAAAA {
			// The host exists, and no other answer is possible: do not wait
			// for the other lookup.
			return dns.RcodeSuccess, nil
		}
		exists = true
	}

	switch {
	case exists:
		return dns.RcodeSuccess, nil
	case failed:
		return dns.RcodeServerFailure, nil
	default:
		return dns.RcodeNameError, nil
	}
}

func addressRecord(name string, res lookupResult) dns.RR {
	hdr := dns.RR_Header{Name: name, Rrtype: res.family.qtype, Class: dns.ClassINET, Ttl: answerTTL}
	if res.family.qtype == dns.TypeA {
		return &dns.A{Hdr: hdr, A: res.address.AsSlice()}
	}
	return &dns.AAAA{Hdr: hdr, AAAA: res.address.AsSlice()}
}

// lookup asks avahi for the address of name in family f. Concurrent lookups of
// the same name and family share one avahi request: a resolver sends its query
// again before a slow lookup ends, and clients ask for A and AAAA together.
func (me *forwarder) lookup(name string, f family) lookupResult {
	key := f.label + " " + strings.ToLower(name)
	res, _, _ := me.inflight.Do(key, func() (any, error) {
		return me.doLookup(name, f), nil
	})
	return res.(lookupResult)
}

func (me *forwarder) doLookup(name string, f family) lookupResult {
	ctx := me.ctx
	if me.timeout != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, me.timeout)
		defer cancel()
	}

	logger := me.logger.WithFields(logrus.Fields{
		"name":     name,
		"type":     f.label,
		"protocol": f.proto,
	})
	logger.Info("forwarding query to avahi")

	res := lookupResult{family: f}
	address, err := me.doAvahiRequest(ctx, name, f.proto)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		logger.Debug("avahi request timed out, host not found")
		return res
	case isAvahiMiss(err):
		logger.WithError(err).Debug("host not found")
		return res
	case err != nil:
		logger.WithError(err).Error("avahi lookup failed")
		res.err = err
		return res
	}

	addr, err := netip.ParseAddr(address)
	if err != nil {
		logger.WithError(err).Error("avahi returned an invalid address")
		res.err = err
		return res
	}
	res.exists = true
	// A link-local IPv6 address needs a zone (the interface), which DNS cannot
	// carry: the client cannot use it.
	if addr.Is6() && addr.IsLinkLocalUnicast() {
		logger.WithField("address", address).Debug("ignoring link-local address")
		return res
	}
	res.address = addr.Unmap()
	return res
}

func isAvahiMiss(err error) bool {
	var dbusErr dbus.Error
	if errors.As(err, &dbusErr) {
		return avahiMissErrors[dbusErr.Name]
	}
	var dbusErrPtr *dbus.Error
	if errors.As(err, &dbusErrPtr) {
		return avahiMissErrors[dbusErrPtr.Name]
	}
	return false
}

// zoneOf returns the configured domain that contains name. The DNS mux only
// sends names under a configured domain to the forwarder.
func (me *forwarder) zoneOf(name string) string {
	lower := strings.ToLower(name)
	for off, end := 0, false; !end; off, end = dns.NextLabel(lower, off) {
		if me.domains[lower[off:]] {
			return lower[off:]
		}
	}
	return lower
}

func (me *forwarder) soaRecord(zone string) dns.RR {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 10},
		Ns:      zone,
		Mbox:    "avahi2dns." + zone,
		Serial:  1,
		Refresh: 60,
		Retry:   60,
		Expire:  3600,
		Minttl:  10,
	}
}

func (me *forwarder) doAvahiRequest(ctx context.Context, name string, proto int32) (string, error) {
	type avahiResult struct {
		address string
		err     error
	}
	resultChan := make(chan avahiResult)
	go func() {
		defer close(resultChan)
		hn, err := me.avahi.ResolveHostName(avahi.InterfaceUnspec, proto, name, proto, 0)
		select {
		case resultChan <- avahiResult{address: hn.Address, err: err}:
		default:
			if err != nil && !isAvahiMiss(err) {
				me.logger.WithError(err).Error("avahi resolve failed (post-timeout)")
			}
		}
	}()
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("avahi request timed out: %w", ctx.Err())
	case result := <-resultChan:
		if result.err != nil {
			return "", fmt.Errorf("avahi resolve failure: %w", result.err)
		}
		return result.address, nil
	}
}
