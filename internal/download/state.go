// Package download is the per-download state machine from design 0001 §7:
//
//	QUEUED → FETCHING → DOWNLOADING → ASSEMBLING → IMPORTING → DONE
//	   ↘ PAUSED (from QUEUED, FETCHING, DOWNLOADING) → QUEUED on resume
//	   ↘ FAILED (from any running state) → QUEUED on retry, or straight back to
//	     ASSEMBLING or IMPORTING when it failed there, with its files on disk
//	   ↘ CANCELLED (from anything but DONE and CANCELLED)
//
// The database is the source of truth. Commands change state inside the
// command's transaction; a runner advances a download through its stages
// with compare-and-set transitions, so a command that lands mid-stage wins
// and the runner simply stops. Every transition writes its DownloadUpdate
// event to the outbox in the same transaction.
package download

import (
	pb "github.com/getnabbed/agent/pkg/proto/nabbed/agent/v2"
)

// State is a download state.
type State = pb.DownloadState

// The download states, named for readability.
const (
	Queued      = pb.DownloadState_DOWNLOAD_STATE_QUEUED
	Fetching    = pb.DownloadState_DOWNLOAD_STATE_FETCHING
	Downloading = pb.DownloadState_DOWNLOAD_STATE_DOWNLOADING
	Assembling  = pb.DownloadState_DOWNLOAD_STATE_ASSEMBLING
	Importing   = pb.DownloadState_DOWNLOAD_STATE_IMPORTING
	Done        = pb.DownloadState_DOWNLOAD_STATE_DONE
	Paused      = pb.DownloadState_DOWNLOAD_STATE_PAUSED
	Failed      = pb.DownloadState_DOWNLOAD_STATE_FAILED
	Cancelled   = pb.DownloadState_DOWNLOAD_STATE_CANCELLED
)

// Running reports whether a runner should be working on s.
func Running(s State) bool {
	switch s {
	case Queued, Fetching, Downloading, Assembling, Importing:
		return true
	}

	return false
}

// next is the runner's forward edge out of each running state.
func next(s State) (State, bool) {
	switch s {
	case Queued:
		return Fetching, true
	case Fetching:
		return Downloading, true
	case Downloading:
		return Assembling, true
	case Assembling:
		return Importing, true
	case Importing:
		return Done, true
	}

	return 0, false
}

// Action is a command-driven transition.
type Action int

// The command-driven actions.
const (
	Pause Action = iota
	Resume
	Retry
	Cancel
)

func (a Action) String() string {
	return [...]string{"pause", "resume", "retry", "cancel"}[a]
}

// Target returns where action moves a download in state from, or false if
// the edge does not exist.
func Target(action Action, from State) (State, bool) {
	switch action {
	case Pause:
		if from == Queued || from == Fetching || from == Downloading {
			return Paused, true
		}
	case Resume:
		if from == Paused {
			return Queued, true
		}
	case Retry:
		if from == Failed {
			return Queued, true
		}
	case Cancel:
		if from != Done && from != Cancelled {
			return Cancelled, true
		}
	}

	return 0, false
}
