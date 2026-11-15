package parcel

import (
	"slices"
	"testing"
)

func TestLinksRefuseNumbersThatAreNotNormalised(t *testing.T) {
	t.Parallel()
	carriers := DefaultCarriers()
	for _, id := range []string{"postnl", "dhl_nl", "dhl", "dpd", "gls", "ups", "fedex"} {
		carrier, _ := carriers.ByID(id)
		if carrier.Link("EX123456789") == "" {
			t.Errorf("%s: no link for a normalised number", id)
		}
		for _, hostile := range []string{"../../x?y=1", "EX1234&x=https://evil.test", "ex123456789", "EX 123456789", "EX123456789/../a"} {
			if link := carrier.Link(hostile); link != "" {
				t.Errorf("%s: link %q for %q", id, link, hostile)
			}
		}
	}
	if dhl, _ := carriers.ByID("dhl_nl"); dhl.Link("JVGL0123456789012345") != "https://my.dhlecommerce.nl/home/tracktrace/JVGL0123456789012345" {
		t.Errorf("DHL Parcel link = %q", dhl.Link("JVGL0123456789012345"))
	}
	if found := carriers.Detect("3SABCD123456789"); len(found) != 2 || found[0].ID != "postnl" || found[1].ID != "dhl_nl" {
		t.Errorf("a 3S number suggests %+v, want PostNL and DHL Parcel", found)
	}
}

func TestValidateRefusesLinksThatAreNotHTTPS(t *testing.T) {
	t.Parallel()
	base := Parcel{Carrier: "postnl", Number: "EX123456789NL", Source: SourceMail, Status: StatusInTransit}
	for link, ok := range map[string]bool{
		"": true, "https://example.test/track/1": true, "http://example.test/": false, "javascript:alert(1)": false,
		"https:///path": false, "//example.test/x": false, "example.test/x": false,
	} {
		item := base
		item.Link = link
		if (item.Validate() == nil) != ok {
			t.Errorf("link %q: valid = %v, want %v", link, item.Validate() == nil, ok)
		}
	}
}

func TestParcelKeysListEveryIdentity(t *testing.T) {
	t.Parallel()
	all := Parcel{Carrier: "postnl", Number: "EX123456789NL", Source: SourceHomeAssistant, SourceKey: "k1", Sender: "Example Shop", OrderRef: "123"}
	want := []string{"postnl|EX123456789NL", "source|ha|k1", "order|Example Shop|123"}
	if !slices.Equal(all.Keys(), want) {
		t.Fatalf("keys = %v, want %v", all.Keys(), want)
	}
	if keys := (Parcel{Carrier: "postnl", Source: SourceMail, OrderRef: "123"}).Keys(); len(keys) != 0 {
		t.Fatalf("an order reference without its sender has keys %v", keys)
	}
}

func TestStatusSupersedes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		incoming, stored Status
		want             bool
	}{
		{StatusInTransit, StatusRegistered, true}, {StatusRegistered, StatusInTransit, false}, {StatusAtPickupPoint, StatusOutForDelivery, true},
		{StatusInTransit, StatusDelivered, false}, {StatusProblem, StatusDelivered, true}, {StatusInTransit, StatusProblem, true},
		{StatusUnknown, StatusRegistered, false}, {StatusDelivered, StatusUnknown, true}, {Status("lost"), StatusRegistered, false},
	} {
		if got := tc.incoming.Supersedes(tc.stored); got != tc.want {
			t.Errorf("%s over %s = %v, want %v", tc.incoming, tc.stored, got, tc.want)
		}
	}
}
