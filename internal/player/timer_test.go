package player

import (
	"testing"
	"time"
)

func TestConsumePacketsTimerLeak(t *testing.T) {
	dropTimer := time.NewTimer(0)
	if !dropTimer.Stop() {
		<-dropTimer.C
	}
	
	droppedCount := 0
	
	// Simulate sending 100 packets
	for i := 0; i < 100; i++ {
		dropTimer.Reset(1 * time.Millisecond)
		
		// Wait long enough for dropTimer to fire
		time.Sleep(2 * time.Millisecond)
		
		// If OpusSend is ready, it could be selected even if dropTimer is fired.
		// Let's force OpusSend to be selected to trigger the Stop() bug.
		// We'll manually call Stop() without draining.
		if !dropTimer.Stop() {
			select {
			case <-dropTimer.C:
			default:
			}
		}
		
		// Now simulate the NEXT iteration where OpusSend might block slightly
		dropTimer.Reset(1 * time.Millisecond)
		select {
		case <-time.After(0 * time.Millisecond): // OpusSend ready immediately
			if !dropTimer.Stop() {
				select {
				case <-dropTimer.C:
				default:
				}
			}
		case <-dropTimer.C:
			// THIS should only happen if OpusSend blocked > 1ms.
			// BUT if the previous iteration leaked, this might happen IMMEDIATELY!
			droppedCount++
		}
	}
	
	if droppedCount > 0 {
		t.Fatalf("Dropped %d packets due to timer leak!", droppedCount)
	}
}
