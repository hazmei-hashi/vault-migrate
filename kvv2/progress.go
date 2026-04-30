package kvv2

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

type ProgressBar struct {
	Total          int
	Completed      int
	Failed         int
	Skipped        int
	Width          int
	lastUpdate     time.Time
	updateThrottle time.Duration
	isTTY          bool
}

func NewProgressBar(total int) *ProgressBar {
	isTTY := term.IsTerminal(int(os.Stdout.Fd()))

	return &ProgressBar{
		Total:          total,
		Width:          40,
		updateThrottle: 100 * time.Millisecond,
		lastUpdate:     time.Time{},
		isTTY:          isTTY,
	}
}

func (pb *ProgressBar) Update(completed, failed, skipped int) {
	pb.Completed = completed
	pb.Failed = failed
	pb.Skipped = skipped
}

func (pb *ProgressBar) ShouldRender() bool {
	if !pb.isTTY {
		return false
	}

	now := time.Now()
	if now.Sub(pb.lastUpdate) < pb.updateThrottle {
		current := pb.Completed + pb.Failed + pb.Skipped
		if current < pb.Total {
			return false
		}
	}
	pb.lastUpdate = now
	return true
}

func (pb *ProgressBar) Render() string {
	current := pb.Completed + pb.Failed + pb.Skipped
	percent := 0.0
	if pb.Total > 0 {
		percent = float64(current) / float64(pb.Total) * 100
	}

	filled := 0
	if pb.Total > 0 {
		filled = int(float64(pb.Width) * float64(current) / float64(pb.Total))
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", pb.Width-filled)

	return fmt.Sprintf("\rProgress: [%s] %.0f%% (%d/%d) - %d completed, %d failed, %d skipped",
		bar, percent, current, pb.Total, pb.Completed, pb.Failed, pb.Skipped)
}

func (pb *ProgressBar) RenderFinal() string {
	return pb.Render()
}

func (pb *ProgressBar) Finish() {
	if pb.isTTY {
		fmt.Println()
	}
}

func (pb *ProgressBar) IsTTY() bool {
	return pb.isTTY
}
