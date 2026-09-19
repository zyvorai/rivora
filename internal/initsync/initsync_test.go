// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
package initsync

import (
	"testing"
	"time"
)

func isDone(t *Tracker) bool {
	select {
	case <-t.Done():
		return true
	default:
		return false
	}
}

func TestTrackerCompletesWhenEveryKeyFinished(t *testing.T) {
	tr := New()
	if isDone(tr) {
		t.Fatal("done before it was armed")
	}
	tr.Finished("a") // before Arm: ignored
	tr.Arm([]string{"a", "b", "c"})
	if isDone(tr) || tr.Pending() != 3 {
		t.Fatalf("pending %d, done %v", tr.Pending(), isDone(tr))
	}
	tr.Finished("a")
	tr.Finished("a") // repeats are harmless
	tr.Finished("zzz")
	if isDone(tr) || tr.Pending() != 2 {
		t.Fatalf("pending %d after one key finished", tr.Pending())
	}
	tr.Finished("b")
	tr.Finished("c")
	select {
	case <-tr.Done():
	case <-time.After(time.Second):
		t.Fatal("not done after every key finished")
	}
}

func TestTrackerWithNothingToWaitForIsDoneAtOnce(t *testing.T) {
	tr := New()
	tr.Arm(nil)
	if !isDone(tr) {
		t.Error("an empty set must complete immediately")
	}
}

func TestTrackerArmsOnce(t *testing.T) {
	tr := New()
	tr.Arm([]string{"a"})
	tr.Arm([]string{"b", "c"}) // ignored
	if tr.Pending() != 1 {
		t.Errorf("a second Arm changed the pending set: %d", tr.Pending())
	}
}
