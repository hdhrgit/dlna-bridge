package ssdp

import (
	"strconv"
	"testing"
	"time"
)

func TestSearch(t *testing.T) {
	st, mx, ok := parseSearch([]byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: urn:schemas-upnp-org:device:MediaServer:1\r\n\r\n"))
	if !ok || mx != 2 || st != "urn:schemas-upnp-org:device:MediaServer:1" {
		t.Fatalf("parse: %q %d %v", st, mx, ok)
	}
	if _, _, ok := parseSearch([]byte("NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\n\r\n")); ok {
		t.Error("a NOTIFY is not a search")
	}
	a := &Advertiser{UUID: "abc"}
	if got := a.answers(st); len(got) != 1 || got[0].usn != "uuid:abc::"+st {
		t.Errorf("answers: %+v", got)
	}
	if got := a.answers("ssdp:all"); len(got) != 5 {
		t.Errorf("ssdp:all: %d answers", len(got))
	}
	if got := a.answers("urn:schemas-upnp-org:device:MediaRenderer:1"); got != nil {
		t.Errorf("renderer search answered: %+v", got)
	}
}

// One address may search in a burst, then at a steady pace; another address has
// its own allowance.
func TestLimiter(t *testing.T) {
	var l limiter
	now := time.Unix(1000, 0)
	for i := 0; i < searchBurst; i++ {
		if !l.allow("10.0.0.5", now) {
			t.Fatalf("search %d of the burst refused", i+1)
		}
	}
	if l.allow("10.0.0.5", now) {
		t.Error("a search over the burst was answered")
	}
	if !l.allow("10.0.0.6", now) {
		t.Error("another address was refused for the first one's searches")
	}
	if l.allow("10.0.0.5", now.Add(searchEvery/2)) {
		t.Error("answered before a token was back")
	}
	if !l.allow("10.0.0.5", now.Add(searchEvery)) {
		t.Error("refused after a token was back")
	}
}

// A flood from more addresses than are kept is refused, not remembered without
// end; quiet ones are forgotten so that real devices are served again.
func TestLimiterFlood(t *testing.T) {
	var l limiter
	now := time.Unix(1000, 0)
	for i := 0; i < maxSenders; i++ {
		if !l.allow("10.1."+strconv.Itoa(i/250)+"."+strconv.Itoa(i%250), now) {
			t.Fatalf("sender %d refused", i)
		}
	}
	if l.allow("10.2.0.1", now) {
		t.Error("a sender over the limit of tracked addresses was answered")
	}
	if len(l.m) > maxSenders {
		t.Errorf("%d senders kept", len(l.m))
	}
	later := now.Add(searchBurst*searchEvery + time.Second)
	if !l.allow("10.2.0.1", later) {
		t.Error("not answered once the quiet senders could be forgotten")
	}
}

// Behind a proxy the announced address is the proxy's, on its port.
func TestAnnouncedLocation(t *testing.T) {
	a := &Advertiser{UUID: "abc", Port: 8080, Host: "192.168.1.10"}
	if got := a.location(group); got != "http://192.168.1.10:8080/description.xml" {
		t.Errorf("location %q", got)
	}
}
