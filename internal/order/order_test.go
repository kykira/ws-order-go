package order

import "testing"

func TestBizResponseIsSuccess(t *testing.T) {
	cases := []struct {
		name string
		resp bizResponse
		want bool
	}{
		{"binance code 000000", bizResponse{Code: "000000"}, true},
		{"binance success flag", bizResponse{Success: true}, true},
		{"turboflow errno 200", bizResponse{Errno: "200", Msg: "success"}, true},
		{"turboflow msg success only", bizResponse{Msg: "SUCCESS"}, true},
		{"empty response", bizResponse{}, false},
		{"binance business error", bizResponse{Code: "93420004"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.resp.isSuccess(); got != tc.want {
				t.Fatalf("isSuccess() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPeriodConversion(t *testing.T) {
	cases := []struct {
		period string
		sec    string
		min    string
	}{
		{"1m", "60", "1"},
		{"3m", "180", "3"},
		{"5m", "300", "5"},
		{"10m", "600", "10"},
		{"15m", "900", "15"},
		{"30m", "1800", "30"},
		{"1h", "3600", "60"},
		{"2h", "7200", "120"},
	}
	for _, tc := range cases {
		t.Run(tc.period, func(t *testing.T) {
			if got := periodToSeconds(tc.period); got != tc.sec {
				t.Fatalf("periodToSeconds(%q) = %q, want %q", tc.period, got, tc.sec)
			}
			if got := periodToMinutes(tc.period); got != tc.min {
				t.Fatalf("periodToMinutes(%q) = %q, want %q", tc.period, got, tc.min)
			}
		})
	}
}
