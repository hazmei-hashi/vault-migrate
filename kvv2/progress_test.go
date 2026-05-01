package kvv2

import (
	"strings"
	"testing"
	"time"
)

func TestNewProgressBar(t *testing.T) {
	pb := NewProgressBar(100)

	if pb.Total != 100 {
		t.Errorf("NewProgressBar() Total = %d, want 100", pb.Total)
	}
	if pb.Width != 40 {
		t.Errorf("NewProgressBar() Width = %d, want 40", pb.Width)
	}
	if pb.updateThrottle != 100*time.Millisecond {
		t.Errorf("NewProgressBar() updateThrottle = %v, want 100ms", pb.updateThrottle)
	}
	if pb.Completed != 0 || pb.Failed != 0 || pb.Skipped != 0 {
		t.Errorf("NewProgressBar() counters not zero: completed=%d, failed=%d, skipped=%d",
			pb.Completed, pb.Failed, pb.Skipped)
	}
}

func TestProgressBar_Update(t *testing.T) {
	pb := NewProgressBar(100)

	pb.Update(50, 3, 2)

	if pb.Completed != 50 {
		t.Errorf("Update() Completed = %d, want 50", pb.Completed)
	}
	if pb.Failed != 3 {
		t.Errorf("Update() Failed = %d, want 3", pb.Failed)
	}
	if pb.Skipped != 2 {
		t.Errorf("Update() Skipped = %d, want 2", pb.Skipped)
	}
}

func TestProgressBar_Render(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		completed int
		failed    int
		skipped   int
		wantBar   string
		wantPct   string
	}{
		{
			name:      "0% progress",
			total:     100,
			completed: 0,
			failed:    0,
			skipped:   0,
			wantBar:   strings.Repeat("░", 40),
			wantPct:   "0%",
		},
		{
			name:      "50% progress",
			total:     100,
			completed: 50,
			failed:    0,
			skipped:   0,
			wantBar:   strings.Repeat("█", 20) + strings.Repeat("░", 20),
			wantPct:   "50%",
		},
		{
			name:      "100% progress",
			total:     100,
			completed: 100,
			failed:    0,
			skipped:   0,
			wantBar:   strings.Repeat("█", 40),
			wantPct:   "100%",
		},
		{
			name:      "mixed progress",
			total:     100,
			completed: 70,
			failed:    5,
			skipped:   5,
			wantBar:   strings.Repeat("█", 32) + strings.Repeat("░", 8),
			wantPct:   "80%",
		},
		{
			name:      "zero total",
			total:     0,
			completed: 0,
			failed:    0,
			skipped:   0,
			wantBar:   strings.Repeat("░", 40),
			wantPct:   "0%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pb := NewProgressBar(tt.total)
			pb.Update(tt.completed, tt.failed, tt.skipped)

			got := pb.Render()

			if !strings.Contains(got, tt.wantBar) {
				t.Errorf("Render() bar = %q, want containing %q", got, tt.wantBar)
			}
			if !strings.Contains(got, tt.wantPct) {
				t.Errorf("Render() percent = %q, want containing %q", got, tt.wantPct)
			}
			if !strings.Contains(got, "completed") {
				t.Errorf("Render() missing 'completed' label")
			}
			if !strings.Contains(got, "failed") {
				t.Errorf("Render() missing 'failed' label")
			}
			if !strings.Contains(got, "skipped") {
				t.Errorf("Render() missing 'skipped' label")
			}
		})
	}
}

func TestProgressBar_RenderFormat(t *testing.T) {
	pb := NewProgressBar(250)
	pb.Update(125, 3, 2)

	got := pb.Render()

	expectedParts := []string{
		"Progress:",
		"52%",
		"(130/250)",
		"125 completed",
		"3 failed",
		"2 skipped",
	}

	for _, part := range expectedParts {
		if !strings.Contains(got, part) {
			t.Errorf("Render() missing expected part %q in output %q", part, got)
		}
	}

	if !strings.HasPrefix(got, "\r") {
		t.Errorf("Render() should start with \\r for in-place update")
	}
}

func TestProgressBar_ShouldRender_Throttling(t *testing.T) {
	pb := NewProgressBar(100)
	pb.isTTY = true

	pb.Update(10, 0, 0)
	shouldRender1 := pb.ShouldRender()
	if !shouldRender1 {
		t.Errorf("ShouldRender() first call = false, want true")
	}

	pb.Update(20, 0, 0)
	shouldRender2 := pb.ShouldRender()
	if shouldRender2 {
		t.Errorf("ShouldRender() immediate second call = true, want false (throttled)")
	}

	time.Sleep(101 * time.Millisecond)

	pb.Update(30, 0, 0)
	shouldRender3 := pb.ShouldRender()
	if !shouldRender3 {
		t.Errorf("ShouldRender() after throttle period = false, want true")
	}
}

func TestProgressBar_ShouldRender_Completion(t *testing.T) {
	pb := NewProgressBar(100)
	pb.isTTY = true

	pb.Update(99, 0, 0)
	pb.ShouldRender()

	pb.Update(100, 0, 0)
	shouldRender := pb.ShouldRender()
	if !shouldRender {
		t.Errorf("ShouldRender() at completion = false, want true (always render on completion)")
	}
}

func TestProgressBar_ShouldRender_NonTTY(t *testing.T) {
	pb := NewProgressBar(100)
	pb.isTTY = false

	pb.Update(50, 0, 0)
	shouldRender := pb.ShouldRender()
	if shouldRender {
		t.Errorf("ShouldRender() non-TTY = true, want false")
	}
}

func TestProgressBar_IsTTY(t *testing.T) {
	pb := NewProgressBar(100)

	got := pb.IsTTY()
	if got != pb.isTTY {
		t.Errorf("IsTTY() = %v, want %v", got, pb.isTTY)
	}
}

func TestProgressBar_RenderFinal(t *testing.T) {
	pb := NewProgressBar(100)
	pb.Update(100, 0, 0)

	render := pb.Render()
	renderFinal := pb.RenderFinal()

	if render != renderFinal {
		t.Errorf("RenderFinal() = %q, want same as Render() = %q", renderFinal, render)
	}
}

func TestProgressBar_Finish(t *testing.T) {
	pb := NewProgressBar(100)
	pb.isTTY = true

	pb.Finish()

	pb.isTTY = false
	pb.Finish()
}
