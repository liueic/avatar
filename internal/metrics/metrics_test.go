package metrics

import (
	"strings"
	"testing"
)

func TestExpositionFormat(t *testing.T) {
	r := New()
	r.Counter("avater_requests_total", "Avatar requests", map[string]string{"result": "approved"}, 3)
	r.Counter("avater_requests_total", "Avatar requests", map[string]string{"result": "default"}, 1)
	r.Counter("avater_requests_total", "Avatar requests", map[string]string{"result": "approved"}, 2)
	r.Gauge("avater_queue_depth", "Queue depth", nil, 42)
	r.Histogram("avater_moderation_duration_seconds", "Classify latency", nil, nil, 0.02)
	r.Histogram("avater_moderation_duration_seconds", "Classify latency", nil, nil, 0.3)

	out := string(r.Render())
	for _, want := range []string{
		"# HELP avater_requests_total Avatar requests",
		"# TYPE avater_requests_total counter",
		`avater_requests_total{result="approved"} 5`,
		`avater_requests_total{result="default"} 1`,
		"# TYPE avater_queue_depth gauge",
		"avater_queue_depth 42",
		"# TYPE avater_moderation_duration_seconds histogram",
		`avater_moderation_duration_seconds_bucket{le="0.025"} 1`,
		`avater_moderation_duration_seconds_bucket{le="0.5"} 2`,
		`avater_moderation_duration_seconds_bucket{le="+Inf"} 2`,
		"avater_moderation_duration_seconds_count 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n---\n%s", want, out)
		}
	}
}

func TestLabelEscaping(t *testing.T) {
	r := New()
	r.Counter("m", "help with \\ and \n newline", map[string]string{`q`: `a"b\c`}, 1)
	out := string(r.Render())
	if !strings.Contains(out, `q="a\"b\\c"`) {
		t.Errorf("label escaping broken: %s", out)
	}
	if !strings.Contains(out, `help with \\ and \n newline`) {
		t.Errorf("help escaping broken: %s", out)
	}
}
