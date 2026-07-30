package promql

import (
	"testing"

	"github.com/tiny-systems/module/module"
)

func TestParseInstantVector(t *testing.T) {
	body := []byte(`{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"pod":"api-1"},"value":[1785000000,"0.75"]},
	  {"metric":{"pod":"api-2"},"value":[1785000000,"0.20"]}]}}`)
	series, truncated, err := parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("two series should not be truncated")
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2", len(series))
	}
	if series[0].Labels["pod"] != "api-1" || series[0].Value != 0.75 {
		t.Errorf("first series = %+v", series[0])
	}
}

// A range query's latest point is what a threshold check reads, so it must be
// promoted to Value rather than leaving the caller to walk the list.
func TestParseRangeUsesNewestAsValue(t *testing.T) {
	body := []byte(`{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"pod":"api-1"},"values":[[1785000000,"0.1"],[1785000060,"0.5"],[1785000120,"0.9"]]}]}}`)
	series, _, err := parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || len(series[0].Points) != 3 {
		t.Fatalf("got %+v", series)
	}
	if series[0].Value != 0.9 {
		t.Errorf("Value = %v, want the newest point 0.9", series[0].Value)
	}
	if series[0].Points[0].At == "" {
		t.Error("timestamp not converted")
	}
}

// The API answers 200 with status:"error" for some failures, so trusting the
// HTTP code alone would hand the caller an empty result instead of the reason.
func TestParseRejectsErrorEnvelope(t *testing.T) {
	body := []byte(`{"status":"error","error":"parse error: unexpected end of input"}`)
	if _, _, err := parse(body); err == nil {
		t.Fatal("expected an error for status:error")
	}
}

func TestClassifyRetryability(t *testing.T) {
	cases := []struct {
		status    int
		retryable bool
	}{
		{400, false}, // a bad query returns 400 forever
		{401, false},
		{422, false},
		{429, true},
		{500, true},
		{503, true},
	}
	for _, c := range cases {
		err := classify(c.status, []byte("boom"))
		if got := module_IsRetryable(err); got != c.retryable {
			t.Errorf("status %d retryable = %v, want %v", c.status, got, c.retryable)
		}
	}
}

func module_IsRetryable(err error) bool { return module.IsRetryable(err) }
