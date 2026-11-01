package web_test

import (
	"net/http"
	"strings"
	"testing"
)

// A precipitation forecast that fails in transit says what kind of failure it was, never the
// request's address, which carries the coordinates.
func TestPrecipitationFailureKeepsTheCoordinatesOut(t *testing.T) {
	f := newFixture(t, "")
	f.route("forecast.test/v1/forecast", func(w http.ResponseWriter, _ *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = connection.Close()
	})
	_, err := f.service.Precipitation(f.ctx, "alice", 52.1234, 5.5678)
	if err == nil {
		t.Fatal("a dropped connection gave a forecast")
	}
	for _, leak := range []string{"52.1234", "5.5678", "latitude", "forecast.test"} {
		if strings.Contains(err.Error(), leak) || strings.Contains(f.logs.String(), leak) {
			t.Fatalf("the failure %q or its log %q names %s", err, f.logs.String(), leak)
		}
	}
}
