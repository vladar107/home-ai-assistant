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

func TestLinkForTakesADutchPostcodeOnlyWhereTheCarrierHasATemplate(t *testing.T) {
	t.Parallel()
	carriers := DefaultCarriers()
	postnl, _ := carriers.ByID("postnl")
	gls, _ := carriers.ByID("gls")
	dhl, _ := carriers.ByID("dhl")
	for _, tc := range []struct {
		carrier          Carrier
		number, postcode string
		want             string
	}{
		{postnl, "3SABCD1234567", "1234 ab", "https://jouw.postnl.nl/track-and-trace/3SABCD1234567-NL-1234AB"},
		{gls, "12345678901234", " 1234AB ", "https://www.gls-info.nl/tracking?trackid=12345678901234&zipcode=1234AB"},
		{postnl, "3SABCD1234567", "", postnl.Link("3SABCD1234567")},
		{postnl, "3SABCD1234567", "12345", postnl.Link("3SABCD1234567")},
		{gls, "12345678901234", "1234AB&x=1", gls.Link("12345678901234")},
		{postnl, "3sabcd1234567", "1234AB", ""},
		{dhl, "1234567890", "1234AB", dhl.Link("1234567890")},
	} {
		if got := tc.carrier.LinkFor(tc.number, tc.postcode); got != tc.want {
			t.Errorf("%s LinkFor(%q, %q) = %q, want %q", tc.carrier.ID, tc.number, tc.postcode, got, tc.want)
		}
	}
	if found := carriers.Detect("12345678901234"); len(found) != 2 || found[0].ID != "dpd" || found[1].ID != "gls" {
		t.Errorf("a 14-digit number suggests %+v, want DPD and GLS", found)
	}
	if carrier, ok := carriers.BySender("noreply@mail.gls-info.nl"); !ok || carrier.ID != "gls" {
		t.Errorf("GLS's Dutch sender = %+v %v", carrier, ok)
	}
}

func TestPlainTextKeepsABareDomainAndDropsAddresses(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"Package from EXAMPLESHOP.NL":          "Package from EXAMPLESHOP.NL",
		"shop.example":                         "shop.example",
		"Kettle from shop.example/":            "Kettle from",
		"Kettle from shop.example/deal now":    "Kettle from now",
		"See https://shop.example or www.a.nl": "See or",
		"Kettle from shop.example?deal=1":      "Kettle from",
		"Kettle from shop.example#deal":        "Kettle from",
		"Kettle from shop.example:8080/deal":   "Kettle from",
	} {
		if got := PlainText(input, 200); got != want {
			t.Errorf("PlainText(%q) = %q, want %q", input, got, want)
		}
	}
}
