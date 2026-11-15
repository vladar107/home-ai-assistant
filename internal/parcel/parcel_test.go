package parcel

import (
	"slices"
	"testing"
)

func TestParseStatusMapsNormalisedStringsAndUnknownValues(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]Status{
		"registered": StatusRegistered, "in_transit": StatusInTransit, " Out-For-Delivery ": StatusOutForDelivery,
		"at pickup point": StatusAtPickupPoint, "delivered": StatusDelivered, "problem": StatusProblem,
		"": StatusUnknown, "exception": StatusUnknown,
	} {
		if got := ParseStatus(input); got != want {
			t.Errorf("ParseStatus(%q) = %q, want %q", input, got, want)
		}
	}
	var noticed []Status
	for _, status := range statuses {
		if status.Notice() {
			noticed = append(noticed, status)
		}
	}
	if want := []Status{StatusOutForDelivery, StatusAtPickupPoint, StatusDelivered, StatusProblem}; !slices.Equal(noticed, want) {
		t.Fatalf("notice-worthy = %v, want %v", noticed, want)
	}
}

func TestNormalizeNumber(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{"ex 1234-56789 nl": "EX123456789NL", "1Z999AA10123456784": "1Z999AA10123456784"} {
		if got, ok := NormalizeNumber(input); !ok || got != want {
			t.Errorf("NormalizeNumber(%q) = %q %v, want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "SHORT1", "EX123456789/NL", "EX 123456789 ÄNL", "A123456789012345678901234567890123456"} {
		if got, ok := NormalizeNumber(input); ok {
			t.Errorf("NormalizeNumber(%q) = %q, want a refusal", input, got)
		}
	}
}

func TestCarriersFindByIDSenderAndNumber(t *testing.T) {
	t.Parallel()
	carriers := DefaultCarriers()
	if carrier, ok := carriers.BySender("Parcels <noreply@mail.postnl.nl>"); !ok || carrier.ID != "postnl" {
		t.Fatalf("sender = %+v %v", carrier, ok)
	}
	if _, ok := carriers.BySender("someone@notpostnl.nl"); ok {
		t.Fatal("a look-alike domain matched")
	}
	if carrier, ok := carriers.ByID("ups"); !ok || carrier.Link("1Z999AA10123456784") != "https://www.ups.com/track?tracknum=1Z999AA10123456784" {
		t.Fatalf("ups = %+v %v", carrier, ok)
	}
	if found := carriers.Detect("1Z999AA10123456784"); len(found) != 1 || found[0].ID != "ups" {
		t.Fatalf("detected = %+v", found)
	}
	shop, _ := carriers.ByID(ShopID)
	if shop.Link("TBA123456789012") != "" || shop.OrderLink("123-4567890-1234567", "ship@mail.amazon.de") != "https://www.amazon.de/gp/your-account/order-details?orderID=123-4567890-1234567" || shop.OrderLink("1/../x", "ship@amazon.de") != "" || shop.OrderLink("123", "ship@example.test") != "" {
		t.Fatalf("shop links = %q %q", shop.Link("TBA123456789012"), shop.OrderLink("1/../x", "ship@mail.amazon.de"))
	}
}

func TestParcelKeyAndValidation(t *testing.T) {
	t.Parallel()
	numbered := Parcel{Carrier: "postnl", Number: "EX123456789NL", Source: SourceMail, Status: StatusInTransit}
	if !slices.Equal(numbered.Keys(), []string{"postnl|EX123456789NL"}) || numbered.Validate() != nil {
		t.Fatalf("numbered = %q %v", numbered.Keys(), numbered.Validate())
	}
	ordered := Parcel{Carrier: ShopID, OrderRef: "123", Sender: "Example Shop", Source: SourceMail, Status: StatusRegistered}
	if !slices.Equal(ordered.Keys(), []string{"order|Example Shop|123"}) || ordered.Validate() != nil {
		t.Fatalf("ordered = %q %v", ordered.Keys(), ordered.Validate())
	}
	for name, broken := range map[string]Parcel{
		"no identity":       {Carrier: "postnl", Source: SourceMail, Status: StatusInTransit},
		"order without who": {Carrier: ShopID, OrderRef: "123", Source: SourceMail, Status: StatusInTransit},
		"raw number":        {Carrier: "postnl", Number: "ex123456789nl", Source: SourceMail, Status: StatusInTransit},
		"bad status":        {Carrier: "postnl", Number: "EX123456789NL", Source: SourceMail, Status: "lost"},
		"bad carrier":       {Carrier: "Post NL", Number: "EX123456789NL", Source: SourceMail, Status: StatusInTransit},
	} {
		if broken.Validate() == nil {
			t.Errorf("%s: validated", name)
		}
	}
}
