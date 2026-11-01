package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/holoplot/go-avahi"
	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testDomainLocal = "local"
	testServerAddr  = "127.0.0.1:15353"
	testServerPort  = 15353
)

func testConfig(mutate func(*config)) *config {
	cfg := &config{
		Domains:  []string{testDomainLocal},
		BindAddr: "127.0.0.1",
		Port:     testServerPort,
	}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func startTestForwarder(t *testing.T, cfg *config, client avahiClient) string {
	t.Helper()

	logger := logrus.New()
	logger.SetOutput(io.Discard)

	fwd, err := NewForwarder(logger, cfg, client)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := fwd.Serve(ctx)
		if err != nil {
			t.Log(err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return testServerAddr
}

func exchange(t *testing.T, addr string, m *dns.Msg) (*dns.Msg, error) {
	t.Helper()

	client := &dns.Client{Timeout: 100 * time.Millisecond}

	var reply *dns.Msg
	var err error
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		reply, _, err = client.Exchange(m, addr)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return reply, err
}

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()

	rr, err := dns.NewRR(s)
	require.NoError(t, err)

	return rr
}

func repack(t *testing.T, msg *dns.Msg) *dns.Msg {
	t.Helper()

	buf, err := msg.Pack()
	require.NoError(t, err)

	result := new(dns.Msg)
	require.NoError(t, result.Unpack(buf))

	return result
}

func assertExchange(t *testing.T, addr string, query, want dns.Msg, wantErr string) {
	t.Helper()

	reply, err := exchange(t, addr, &query)

	if wantErr != "" {
		require.Error(t, err)
		assert.Contains(t, err.Error(), wantErr)
		return
	}

	require.NoError(t, err)
	require.NotNil(t, reply)

	want.Id = reply.Id
	want.Response = true
	if want.Rcode != dns.RcodeFormatError && want.Rcode != dns.RcodeNotImplemented {
		want.Question = query.Question
	}
	assert.Equal(t, repack(t, &want), reply)
}

var (
	// The reply of avahi when no host answers.
	errAvahiMiss = dbus.Error{Name: "org.freedesktop.Avahi.TimeoutError", Body: []any{"Timeout reached"}}
	// The reply of the bus when avahi does not run.
	errAvahiDown = dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown", Body: []any{"The name is not activatable"}}
)

func expectLookup(client *mockavahiClient, name string, proto int32, address string, err error) *mockavahiClient_ResolveHostName_Call {
	return client.EXPECT().
		ResolveHostName(int32(avahi.InterfaceUnspec), proto, name, proto, uint32(0)).
		Return(avahi.HostName{Address: address}, err)
}

func question(name string, qtype uint16) dns.Msg {
	return dns.Msg{Question: []dns.Question{{Name: name, Qtype: qtype, Qclass: dns.ClassINET}}}
}

func answerMsg(rcode int, answer ...dns.RR) dns.Msg {
	return dns.Msg{
		MsgHdr: dns.MsgHdr{Opcode: dns.OpcodeQuery, Authoritative: true, Rcode: rcode},
		Answer: answer,
	}
}

func negativeMsg(t *testing.T, rcode int) dns.Msg {
	return dns.Msg{
		MsgHdr: dns.MsgHdr{Opcode: dns.OpcodeQuery, Authoritative: true, Rcode: rcode},
		Ns:     []dns.RR{mustRR(t, "local. 10 SOA local. avahi2dns.local. 1 60 60 3600 10")},
	}
}

func TestForwarder(t *testing.T) {
	v4, v6 := int32(avahi.ProtoInet), int32(avahi.ProtoInet6)

	testCases := []struct {
		name      string
		cfg       func(c *config)
		query     dns.Msg
		setupMock func(client *mockavahiClient)
		want      dns.Msg
		wantErr   string
	}{
		{
			name:  "SOA apex of the only configured domain",
			query: question("local.", dns.TypeSOA),
			want:  answerMsg(dns.RcodeSuccess, mustRR(t, "local. 10 SOA local. avahi2dns.local. 1 60 60 3600 10")),
		},
		{
			name:  "SOA apex of a custom domain list",
			cfg:   func(c *config) { c.Domains = []string{"foo", "bar"} },
			query: question("bar.", dns.TypeSOA),
			want:  answerMsg(dns.RcodeSuccess, mustRR(t, "bar. 10 SOA bar. avahi2dns.bar. 1 60 60 3600 10")),
		},
		{
			name:  "A at the apex is NODATA without a lookup",
			query: question("local.", dns.TypeA),
			want:  negativeMsg(t, dns.RcodeSuccess),
		},
		{
			name:  "SOA for a non-supported apex",
			query: question("example.com.", dns.TypeSOA),
			want: dns.Msg{
				MsgHdr: dns.MsgHdr{Opcode: dns.OpcodeQuery, Rcode: dns.RcodeRefused},
			},
		},
		{
			name:  "A record resolves",
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "192.168.1.42", nil)
				expectLookup(client, "host.local.", v6, "", errAvahiMiss).Maybe()
			},
			want: answerMsg(dns.RcodeSuccess, mustRR(t, "host.local. A 192.168.1.42")),
		},
		{
			name:  "AAAA record resolves",
			query: question("host.local.", dns.TypeAAAA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "", errAvahiMiss).Maybe()
				expectLookup(client, "host.local.", v6, "fd00::42", nil)
			},
			want: answerMsg(dns.RcodeSuccess, mustRR(t, "host.local. AAAA fd00::42")),
		},
		{
			name:  "link-local AAAA is not sent, the host exists: NODATA",
			query: question("host.local.", dns.TypeAAAA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "", errAvahiMiss)
				expectLookup(client, "host.local.", v6, "fe80::1", nil)
			},
			want: negativeMsg(t, dns.RcodeSuccess),
		},
		{
			name:  "AAAA of a host with only IPv4 is NODATA",
			query: question("host.local.", dns.TypeAAAA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "192.168.1.42", nil)
				expectLookup(client, "host.local.", v6, "", errAvahiMiss)
			},
			want: negativeMsg(t, dns.RcodeSuccess),
		},
		{
			name:  "unknown host is NXDOMAIN",
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "", errAvahiMiss)
				expectLookup(client, "host.local.", v6, "", errAvahiMiss)
			},
			want: negativeMsg(t, dns.RcodeNameError),
		},
		{
			name:  "avahi NotFoundError is a miss",
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "", dbus.Error{Name: "org.freedesktop.Avahi.NotFoundError"})
				expectLookup(client, "host.local.", v6, "", errAvahiMiss)
			},
			want: negativeMsg(t, dns.RcodeNameError),
		},
		{
			name:  "lookups that time out are a miss",
			cfg:   func(c *config) { c.Timeout = 10 * time.Millisecond },
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				for _, proto := range []int32{v4, v6} {
					expectLookup(client, "host.local.", proto, "192.168.1.1", nil).
						Run(func(_ int32, _ int32, _ string, _ int32, _ uint32) {
							time.Sleep(100 * time.Millisecond)
						})
				}
			},
			want: negativeMsg(t, dns.RcodeNameError),
		},
		{
			name:  "avahi not running is SERVFAIL",
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "", errAvahiDown)
				expectLookup(client, "host.local.", v6, "", errAvahiDown)
			},
			want: answerMsg(dns.RcodeServerFailure),
		},
		{
			name:  "a miss and a failure is SERVFAIL",
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "", errAvahiMiss)
				expectLookup(client, "host.local.", v6, "", errors.New("connection reset"))
			},
			want: answerMsg(dns.RcodeServerFailure),
		},
		{
			name:  "a failure and an existing host is NODATA",
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "", errAvahiDown)
				expectLookup(client, "host.local.", v6, "fd00::42", nil)
			},
			want: negativeMsg(t, dns.RcodeSuccess),
		},
		{
			name:  "an invalid address from avahi is a failure",
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "not-an-address", nil)
				expectLookup(client, "host.local.", v6, "", errAvahiMiss)
			},
			want: answerMsg(dns.RcodeServerFailure),
		},
		{
			name:  "A query when v6only and the host has IPv6 is NODATA",
			cfg:   func(c *config) { c.V6only = true },
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v6, "fd00::42", nil)
			},
			want: negativeMsg(t, dns.RcodeSuccess),
		},
		{
			name:  "A query when v6only and no host is NXDOMAIN",
			cfg:   func(c *config) { c.V6only = true },
			query: question("host.local.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v6, "", errAvahiMiss)
			},
			want: negativeMsg(t, dns.RcodeNameError),
		},
		{
			name:  "AAAA query when v4only and the host has IPv4 is NODATA",
			cfg:   func(c *config) { c.V4only = true },
			query: question("host.local.", dns.TypeAAAA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "192.168.1.42", nil)
			},
			want: negativeMsg(t, dns.RcodeSuccess),
		},
		{
			name:  "other type for an existing host is NODATA",
			query: question("host.local.", dns.TypeHTTPS),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "192.168.1.42", nil)
				expectLookup(client, "host.local.", v6, "", errAvahiMiss).Maybe()
			},
			want: negativeMsg(t, dns.RcodeSuccess),
		},
		{
			name:  "other type for an unknown host is NXDOMAIN",
			query: question("host.local.", dns.TypeTXT),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.local.", v4, "", errAvahiMiss)
				expectLookup(client, "host.local.", v6, "", errAvahiMiss)
			},
			want: negativeMsg(t, dns.RcodeNameError),
		},
		{
			name:  "negative answer has the SOA of the matching domain",
			cfg:   func(c *config) { c.Domains = []string{"local", "home.arpa"} },
			query: question("host.home.arpa.", dns.TypeA),
			setupMock: func(client *mockavahiClient) {
				expectLookup(client, "host.home.arpa.", v4, "", errAvahiMiss)
				expectLookup(client, "host.home.arpa.", v6, "", errAvahiMiss)
			},
			want: dns.Msg{
				MsgHdr: dns.MsgHdr{Opcode: dns.OpcodeQuery, Authoritative: true, Rcode: dns.RcodeNameError},
				Ns:     []dns.RR{mustRR(t, "home.arpa. 10 SOA home.arpa. avahi2dns.home.arpa. 1 60 60 3600 10")},
			},
		},
		{
			name:  "unconfigured domain",
			query: question("example.com.", dns.TypeA),
			want: dns.Msg{
				MsgHdr: dns.MsgHdr{Opcode: dns.OpcodeQuery, Rcode: dns.RcodeRefused},
			},
		},
		{
			name: "multiple questions is rejected",
			query: dns.Msg{Question: []dns.Question{
				{Name: "host.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
				{Name: "host.local.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET},
			}},
			want: dns.Msg{
				MsgHdr: dns.MsgHdr{Rcode: dns.RcodeFormatError},
			},
		},
		{
			name: "zero questions is rejected",
			want: dns.Msg{
				MsgHdr: dns.MsgHdr{Rcode: dns.RcodeFormatError},
			},
		},
		{
			name:  "unsupported opcode is rejected",
			query: dns.Msg{MsgHdr: dns.MsgHdr{Opcode: dns.OpcodeStatus}, Question: []dns.Question{{Name: "host.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}}},
			want: dns.Msg{
				MsgHdr: dns.MsgHdr{Opcode: dns.OpcodeStatus, Rcode: dns.RcodeNotImplemented},
			},
		},
		{
			name:    "a response message sent as a request is ignored",
			query:   dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Question: []dns.Question{{Name: "host.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}}},
			wantErr: "i/o timeout",
		},
	}

	for _, testcase := range testCases {
		t.Run(testcase.name, func(t *testing.T) {
			client := newMockavahiClient(t)
			if testcase.setupMock != nil {
				testcase.setupMock(client)
			}

			addr := startTestForwarder(t, testConfig(testcase.cfg), client)

			assertExchange(t, addr, testcase.query, testcase.want, testcase.wantErr)
		})
	}
}

func TestForwarderDoesNotWaitForTheOtherFamily(t *testing.T) {
	client := newMockavahiClient(t)
	expectLookup(client, "host.local.", int32(avahi.ProtoInet), "192.168.1.42", nil)
	// The AAAA lookup of a host with only IPv4 lasts until the timeout.
	expectLookup(client, "host.local.", int32(avahi.ProtoInet6), "", errAvahiMiss).
		Run(func(_ int32, _ int32, _ string, _ int32, _ uint32) {
			time.Sleep(500 * time.Millisecond)
		}).
		Maybe()

	addr := startTestForwarder(t, testConfig(nil), client)

	query := question("host.local.", dns.TypeA)
	start := time.Now()
	assertExchange(t, addr, query, answerMsg(dns.RcodeSuccess, mustRR(t, "host.local. A 192.168.1.42")), "")
	assert.Less(t, time.Since(start), 300*time.Millisecond)
}

func TestForwarderSharesConcurrentLookups(t *testing.T) {
	client := newMockavahiClient(t)
	slow := func(_ int32, _ int32, _ string, _ int32, _ uint32) {
		time.Sleep(200 * time.Millisecond)
	}
	// One avahi request per family for all the queries below. The request
	// has the name of the first query, in any case.
	anyCase := mock.MatchedBy(func(name string) bool { return strings.EqualFold(name, "host.local.") })
	for _, proto := range []int32{avahi.ProtoInet, avahi.ProtoInet6} {
		client.EXPECT().
			ResolveHostName(int32(avahi.InterfaceUnspec), proto, anyCase, proto, uint32(0)).
			Return(avahi.HostName{}, errAvahiMiss).
			Run(slow).
			Once()
	}

	addr := startTestForwarder(t, testConfig(nil), client)
	// Wait for the server: the queries below are sent only once.
	ready := question("local.", dns.TypeSOA)
	_, err := exchange(t, addr, &ready)
	require.NoError(t, err)

	// A client asks for A and AAAA, and the resolver sends each query again.
	// The names differ in case only.
	queries := []dns.Msg{
		question("host.local.", dns.TypeA),
		question("host.local.", dns.TypeAAAA),
		question("HOST.local.", dns.TypeA),
		question("host.LOCAL.", dns.TypeAAAA),
	}
	for i, query := range queries {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()

			reply, err := exchangeOnce(t, addr, &query, time.Second)
			require.NoError(t, err)
			assert.Equal(t, dns.RcodeNameError, reply.Rcode)
		})
	}
}

// exchangeOnce sends query once; exchange sends it again on a timeout.
func exchangeOnce(t *testing.T, addr string, query *dns.Msg, timeout time.Duration) (*dns.Msg, error) {
	t.Helper()

	client := &dns.Client{Timeout: timeout}
	reply, _, err := client.Exchange(query, addr)
	return reply, err
}
