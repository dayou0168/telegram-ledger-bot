package p2p

import (
	"encoding/json"
	"testing"
)

func TestOrderBookPriceAcceptsStringOrNumber(t *testing.T) {
	cases := map[string]string{
		`{"price":"6.73"}`: "6.73",
		`{"price":6.73}`:   "6.73",
	}
	for raw, want := range cases {
		var ad orderBookAd
		if err := json.Unmarshal([]byte(raw), &ad); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		if got := ad.Price.String(); got != want {
			t.Fatalf("price for %s = %q, want %q", raw, got, want)
		}
	}
}
