package googlesim

import (
	"bytes"
	"cmp"
	_ "embed"
	"strings"
	"time"
)

// taggedDelivery is a scrubbed copy of a real message as Gmail stored it after delivery to a
// plus-tagged address: the tag survives in Delivered-To, in To, and in the Received "for" clause,
// and header names keep the sender's spelling, such as Message-Id. deliveryHeaders follows its
// shape, so both change together when Gmail's behavior is checked again.
//
//go:embed testdata/tagged-delivery.eml
var taggedDelivery []byte

// SampleDelivery is the delivered message of the fixture, addressed to
// assistant+test1@example.com, with DKIM, SPF and DMARC passing. Give it to DeliverRaw.
func SampleDelivery() []byte {
	return bytes.Clone(taggedDelivery)
}

// SampleDeliveryFailingDMARC is SampleDelivery with Authentication-Results reporting dmarc=fail.
func SampleDeliveryFailingDMARC() []byte {
	return bytes.Replace(taggedDelivery, []byte("dmarc=pass"), []byte("dmarc=fail"), 1)
}

// AuthenticationResults is the Authentication-Results header Gmail adds to mail from the sender
// address, with dmarc the DMARC result, such as pass or fail.
func AuthenticationResults(sender, dmarc string) string {
	_, domain, _ := strings.Cut(sender, "@")
	return "mx.google.com; dkim=pass header.i=@" + domain + ` header.s=20260707 header.b="AbCd"; spf=pass (google.com: domain of ` +
		sender + " designates 192.0.2.41 as permitted sender) smtp.mailfrom=" + sender + "; dmarc=" + dmarc +
		" (p=NONE sp=QUARANTINE dis=NONE) header.from=" + domain
}

// deliveryHeaders is the header block Gmail stores for a message delivered to its Delivered-To
// address, in the order of the fixture.
func deliveryHeaders(message Message) []Header {
	date := message.Received.Format(time.RFC1123Z)
	sender := plainAddress(message.From)
	_, domain, _ := strings.Cut(sender, "@")
	headers := []Header{
		{"Delivered-To", message.DeliveredTo},
		{"Received", "by 2001:db8::1 with SMTP id googlesim; " + date},
		{"Authentication-Results", cmp.Or(message.AuthenticationResults, AuthenticationResults(sender, "pass"))},
		{"Return-Path", "<" + sender + ">"},
		{"Received", "from mail." + domain + " by mx.google.com with SMTPS id googlesim for <" + message.DeliveredTo + "> (Google Transport Security); " + date},
		{"From", message.From},
	}
	if message.ReplyTo != "" {
		headers = append(headers, Header{"Reply-To", message.ReplyTo})
	}
	headers = append(headers,
		Header{"Mime-Version", "1.0"},
		Header{"Subject", message.Subject},
		Header{"Message-Id", message.MessageID},
		Header{"Date", date},
		Header{"To", message.To},
	)
	for _, optional := range []Header{{"Cc", message.Cc}, {"In-Reply-To", message.InReplyTo}, {"References", message.References}} {
		if optional.Value != "" {
			headers = append(headers, optional)
		}
	}
	return append(headers, message.Extra...)
}
