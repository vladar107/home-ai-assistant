package parcel

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var numberPattern = regexp.MustCompile(`^[A-Z0-9]{8,35}$`)

// NormalizeNumber upper-cases a tracking number and strips spaces and hyphens. It reports false
// when what remains is not 8 to 35 letters and digits.
func NormalizeNumber(value string) (string, bool) {
	value = strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(value))
	return value, numberPattern.MatchString(value)
}

// Carrier is one carrier, or the online shop whose own deliveries have an order page instead of a
// tracking page.
type Carrier struct {
	ID   string
	Name string
	// Domains are the sender domains of its mail; a subdomain matches too.
	Domains []string
	// Patterns recognise its numbers, which several carriers can share; they only suggest a carrier.
	Patterns []*regexp.Regexp
	// LinkTemplate is the tracking page with {number} for the normalised number. Empty means the
	// carrier has no link.
	LinkTemplate string
	// PostcodeLinkTemplate is the tracking page that also takes the recipient's postcode, with
	// {number} and {postcode}, so it opens without asking for it. It is used only for a postcode of
	// PostcodePattern's shape, since the template belongs to one country.
	PostcodeLinkTemplate string
	PostcodePattern      *regexp.Regexp
	// OrderLinkTemplate is the order page with {order} for the order reference, for the shop.
	OrderLinkTemplate string
}

// Link is the carrier's tracking page for the number, or "" when it has no template or the number
// is not a normalised one.
func (c Carrier) Link(number string) string {
	if normalised, ok := NormalizeNumber(number); c.LinkTemplate == "" || !ok || normalised != number {
		return ""
	}
	return strings.ReplaceAll(c.LinkTemplate, "{number}", url.PathEscape(number))
}

// LinkFor is the carrier's tracking page for the number and the recipient's postcode. Without a
// postcode template, or for a postcode that is not of its shape, it is Link.
func (c Carrier) LinkFor(number, postcode string) string {
	postcode = strings.ToUpper(strings.Join(strings.Fields(postcode), ""))
	normalised, ok := NormalizeNumber(number)
	if c.PostcodeLinkTemplate == "" || c.PostcodePattern == nil || !ok || normalised != number || !c.PostcodePattern.MatchString(postcode) {
		return c.Link(number)
	}
	return fillLink(c.PostcodeLinkTemplate, map[string]string{"{number}": number, "{postcode}": postcode})
}

// fillLink puts the values into the template, escaped for the path before its query and for the
// query after it.
func fillLink(template string, values map[string]string) string {
	path, query, hasQuery := strings.Cut(template, "?")
	var pathValues, queryValues []string
	for placeholder, value := range values {
		pathValues = append(pathValues, placeholder, url.PathEscape(value))
		queryValues = append(queryValues, placeholder, url.QueryEscape(value))
	}
	filled := strings.NewReplacer(pathValues...).Replace(path)
	if hasQuery {
		filled += "?" + strings.NewReplacer(queryValues...).Replace(query)
	}
	return filled
}

// OrderLink is the shop's order page for the order reference, on the marketplace the sender's
// address belongs to, such as an address at amazon.de. It is "" when the carrier has no template,
// the sender is not one of its domains, or the reference holds anything but letters, digits, and
// hyphens. {host} in the template is the marketplace's domain.
func (c Carrier) OrderLink(order, sender string) string {
	if c.OrderLinkTemplate == "" || order == "" || strings.Trim(order, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return ""
	}
	domain := strings.ToLower(strings.TrimSpace(sender))
	if at := strings.LastIndex(domain, "@"); at >= 0 {
		domain = domain[at+1:]
	}
	domain = strings.TrimSuffix(strings.TrimSuffix(domain, ">"), ".")
	for _, known := range c.Domains {
		if domain == known || strings.HasSuffix(domain, "."+known) {
			return strings.NewReplacer("{host}", known, "{order}", url.QueryEscape(order)).Replace(c.OrderLinkTemplate)
		}
	}
	return ""
}

// Carriers is a list of carriers.
type Carriers []Carrier

// ShopID is the online shop's entry in the default list.
const ShopID = "amazon"

// dutchPostcode is a Dutch postcode with the space between its digits and letters removed.
var dutchPostcode = regexp.MustCompile(`^[0-9]{4}[A-Z]{2}$`)

// DefaultCarriers is the list the assistant knows. A template is left empty where the carrier's
// public tracking address is not certain.
func DefaultCarriers() Carriers {
	return Carriers{
		{
			ID: "postnl", Name: "PostNL", Domains: []string{"postnl.nl"},
			Patterns:     []*regexp.Regexp{regexp.MustCompile(`^3S[A-Z0-9]{11,13}$`), regexp.MustCompile(`^[A-Z]{2}[0-9]{9}NL$`)},
			LinkTemplate: "https://postnl.nl/tracktrace/?B={number}&D=NL&T=C",
			// The form the carrier's own integration hands out.
			PostcodeLinkTemplate: "https://jouw.postnl.nl/track-and-trace/{number}-NL-{postcode}", PostcodePattern: dutchPostcode,
		},
		{
			ID: "dhl_nl", Name: "DHL Parcel", Domains: []string{"dhlparcel.nl", "dhlecommerce.nl"},
			Patterns:     []*regexp.Regexp{regexp.MustCompile(`^JVGL[0-9]{16,20}$`), regexp.MustCompile(`^3S[A-Z0-9]{11,13}$`)},
			LinkTemplate: "https://my.dhlecommerce.nl/home/tracktrace/{number}",
		},
		{
			ID: "dhl", Name: "DHL Express", Domains: []string{"dhl.com", "dhl.nl"},
			Patterns:     []*regexp.Regexp{regexp.MustCompile(`^[0-9]{10}$`), regexp.MustCompile(`^JJD[0-9]{18}$`)},
			LinkTemplate: "https://www.dhl.com/nl-en/home/tracking.html?tracking-id={number}",
		},
		{
			ID: "dpd", Name: "DPD", Domains: []string{"dpd.com", "dpd.nl", "dpd.de", "dpdgroup.com"},
			Patterns:     []*regexp.Regexp{regexp.MustCompile(`^[0-9]{14}$`)},
			LinkTemplate: "https://tracking.dpd.de/status/en_US/parcel/{number}",
		},
		{
			ID: "gls", Name: "GLS", Domains: []string{"gls-group.eu", "gls-group.com", "gls-netherlands.com", "gls-info.nl"},
			// Its 14-digit parcel numbers look like DPD's, so mail settles them by the carrier it names.
			Patterns:     []*regexp.Regexp{regexp.MustCompile(`^[0-9]{11,12}$`), regexp.MustCompile(`^[0-9]{14}$`)},
			LinkTemplate: "https://gls-group.com/EU/en/parcel-tracking?match={number}",
			// The Dutch tracker, as the carrier's integration builds it.
			PostcodeLinkTemplate: "https://www.gls-info.nl/tracking?trackid={number}&zipcode={postcode}", PostcodePattern: dutchPostcode,
		},
		{
			ID: "ups", Name: "UPS", Domains: []string{"ups.com"},
			Patterns:     []*regexp.Regexp{regexp.MustCompile(`^1Z[A-Z0-9]{16}$`)},
			LinkTemplate: "https://www.ups.com/track?tracknum={number}",
		},
		{
			ID: "fedex", Name: "FedEx", Domains: []string{"fedex.com"},
			Patterns:     []*regexp.Regexp{regexp.MustCompile(`^([0-9]{12}|[0-9]{15})$`)},
			LinkTemplate: "https://www.fedex.com/fedextrack/?trknbr={number}",
		},
		{
			ID: ShopID, Name: "Amazon", Domains: []string{"amazon.nl", "amazon.com", "amazon.de", "amazon.co.uk", "amazon.fr", "amazon.es", "amazon.it"},
			Patterns:          []*regexp.Regexp{regexp.MustCompile(`^TBA[0-9]{12}$`)},
			OrderLinkTemplate: "https://www.{host}/gp/your-account/order-details?orderID={order}",
		},
	}
}

// ByID returns the carrier with the ID.
func (c Carriers) ByID(id string) (Carrier, bool) {
	for _, carrier := range c {
		if carrier.ID == id {
			return carrier, true
		}
	}
	return Carrier{}, false
}

// BySender returns the carrier whose domain the address, or a bare domain, belongs to.
func (c Carriers) BySender(address string) (Carrier, bool) {
	domain := strings.ToLower(strings.TrimSpace(address))
	if at := strings.LastIndex(domain, "@"); at >= 0 {
		domain = domain[at+1:]
	}
	domain = strings.TrimSuffix(strings.TrimSuffix(domain, ">"), ".")
	for _, carrier := range c {
		for _, known := range carrier.Domains {
			if domain == known || strings.HasSuffix(domain, "."+known) {
				return carrier, true
			}
		}
	}
	return Carrier{}, false
}

// Detect returns the carriers whose number patterns match the normalised number.
func (c Carriers) Detect(number string) []Carrier {
	var found []Carrier
	for _, carrier := range c {
		for _, pattern := range carrier.Patterns {
			if pattern.MatchString(number) {
				found = append(found, carrier)
				break
			}
		}
	}
	return found
}

// Settle is the carrier a number is stored under. A number whose pattern several carriers share
// keeps the named carrier when it is one of them and otherwise goes to the first of them, so a
// parcel with such a number always stands under a carrier of that set, and never under "other".
func (c Carriers) Settle(carrier, number string) string {
	detected := c.Detect(number)
	if len(detected) < 2 || slices.ContainsFunc(detected, func(found Carrier) bool { return found.ID == carrier }) {
		return carrier
	}
	return detected[0].ID
}

// Twins returns the carriers a number of the carrier can stand under. A number whose pattern
// several carriers share is the same parcel whichever of them a source names, since mail can only
// guess between them: it stands under each of them, the carrier first. Otherwise it is the carrier
// alone.
func (c Carriers) Twins(carrier, number string) []string {
	detected := c.Detect(number)
	if len(detected) < 2 {
		return []string{carrier}
	}
	twins := []string{carrier}
	for _, found := range detected {
		if found.ID != carrier {
			twins = append(twins, found.ID)
		}
	}
	return twins
}

// Domains returns every sender domain of the list, for the mail job's query.
func (c Carriers) Domains() []string {
	var domains []string
	for _, carrier := range c {
		domains = append(domains, carrier.Domains...)
	}
	return domains
}
