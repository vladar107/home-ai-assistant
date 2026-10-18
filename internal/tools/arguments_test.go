package tools

import (
	"encoding/json"
	"testing"
)

func TestDecodeArguments(t *testing.T) {
	type input struct {
		Name  string `json:"name"`
		Limit int    `json:"limit"`
	}
	for _, test := range []struct {
		raw     string
		want    input
		wantErr bool
	}{
		{raw: `{"name":"a","limit":2}`, want: input{Name: "a", Limit: 2}},
		{raw: ``, want: input{}},
		{raw: " \n\t", want: input{}},
		{raw: `{"name":"a","extra":true}`, wantErr: true},
		{raw: `{"limit":"two"}`, wantErr: true},
		{raw: `[1]`, wantErr: true},
		{raw: `{"name":`, wantErr: true},
	} {
		var got input
		err := DecodeArguments(json.RawMessage(test.raw), &got)
		if test.wantErr {
			if err == nil || err.Error() != "the arguments do not match the tool's schema" {
				t.Errorf("DecodeArguments(%q) error = %v", test.raw, err)
			}
			continue
		}
		if err != nil || got != test.want {
			t.Errorf("DecodeArguments(%q) = %+v, %v; want %+v", test.raw, got, err, test.want)
		}
	}
}
