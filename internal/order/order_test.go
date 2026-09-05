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
