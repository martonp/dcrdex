// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package mm

import (
	"fmt"
	"time"

	"decred.org/dcrdex/client/core"
	"decred.org/dcrdex/client/db"
	"decred.org/dcrdex/dex"
)

const (
	validationNote = "validation"
	runStats       = "runstats"
	runEvent       = "runevent"
	botError       = "botError"
)

func newValidationErrorNote(host string, baseID, quoteID uint32, errorMsg string) *db.Notification {
	baseSymbol := dex.BipIDSymbol(baseID)
	quoteSymbol := dex.BipIDSymbol(quoteID)
	msg := fmt.Sprintf("%s-%s @ %s: %s", baseSymbol, quoteSymbol, host, errorMsg)
	note := db.NewNotification(validationNote, "", "Bot Config Validation Error", msg, db.ErrorLevel)
	return &note
}

type runStatsNote struct {
	db.Notification

	Host      string    `json:"host"`
	Base      uint32    `json:"base"`
	Quote     uint32    `json:"quote"`
	StartTime int64     `json:"startTime"`
	Stats     *RunStats `json:"stats"`
}

func newRunStatsNote(host string, base, quote uint32, stats *RunStats) *runStatsNote {
	return &runStatsNote{
		Notification: db.NewNotification(runStats, "", "", "", db.Data),
		Host:         host,
		Base:         base,
		Quote:        quote,
		Stats:        stats,
	}
}

type runEventNote struct {
	db.Notification

	Host      string             `json:"host"`
	Base      uint32             `json:"base"`
	Quote     uint32             `json:"quote"`
	StartTime int64              `json:"startTime"`
	Event     *MarketMakingEvent `json:"event"`
}

func newRunEventNote(host string, base, quote uint32, startTime int64, event *MarketMakingEvent) *runEventNote {
	return &runEventNote{
		Notification: db.NewNotification(runEvent, "", "", "", db.Data),
		Host:         host,
		Base:         base,
		Quote:        quote,
		StartTime:    startTime,
		Event:        event,
	}
}

type botErrorNote struct {
	db.Notification
}

func botSubject(mkt *MarketWithHost) string {
	return fmt.Sprintf("Bot: %s-%s @ %s", dex.BipIDSymbol(mkt.BaseID), dex.BipIDSymbol(mkt.QuoteID), mkt.Host)
}

func newBotErrorNote(mkt *MarketWithHost, err string, warningLevel bool) *botErrorNote {
	level := db.ErrorLevel
	if warningLevel {
		level = db.WarningLevel
	}
	return &botErrorNote{
		Notification: db.NewNotification(botError, "", botSubject(mkt), err, level),
	}
}

type botTimedError struct {
	msg       string
	warn      bool // if false, error level
	timestamp time.Time
}

func newBotTimedError(msg string, warn bool) *botTimedError {
	return &botTimedError{
		msg:       msg,
		warn:      warn,
		timestamp: time.Now(),
	}
}

var rebroadcastInterval = time.Minute * 5

// broadcastNewErrors compares the old and new errors, broadcasting any new
// errors and returning the new list of errors. Errors are considered new if
// they are not in the old list, or if they are in the old list but the last
// broadcast was more than rebroadcastInterval ago.
func broadcastNewErrors(oldErrs, newErrs []*botTimedError, mkt *MarketWithHost, broadcast func(core.Notification)) []*botTimedError {
	toReturn := make([]*botTimedError, 0, len(newErrs))

	for _, newErr := range newErrs {
		var matchingOldError *botTimedError
		for _, oldErr := range oldErrs {
			if oldErr.msg == newErr.msg {
				matchingOldError = oldErr
				break
			}
		}

		if matchingOldError == nil {
			broadcast(newBotErrorNote(mkt, newErr.msg, newErr.warn))
			toReturn = append(toReturn, newErr)
			continue
		}

		if time.Since(matchingOldError.timestamp) > rebroadcastInterval {
			broadcast(newBotErrorNote(mkt, newErr.msg, newErr.warn))
			toReturn = append(toReturn, newErr)
		} else {
			toReturn = append(toReturn, matchingOldError)
		}
	}

	return toReturn
}
