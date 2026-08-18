// Package main (internal helpers) — tiny logr adapter so controller-runtime
// logs flow to the standard log package (the service uses log.Printf; without
// a logger controller-runtime silently drops reconcile errors).
package main

import (
	"log"

	"github.com/go-logr/logr"
)

// stdLogr is a logr.LogSink backed by the standard log package.
type stdLogr struct {
	name string
}

var _ logr.LogSink = (*stdLogr)(nil)

func (l *stdLogr) Init(info logr.RuntimeInfo)        {}
func (l *stdLogr) Enabled(level int) bool            { return true }
func (l *stdLogr) Info(level int, msg string, kv ...any) {
	if l.name != "" {
		log.Printf("ctrl[%s]: %s %v", l.name, msg, kv)
		return
	}
	log.Printf("ctrl: %s %v", msg, kv)
}
func (l *stdLogr) Error(err error, msg string, kv ...any) {
	if l.name != "" {
		log.Printf("ctrl[%s]: ERROR %s: %v %v", l.name, msg, err, kv)
		return
	}
	log.Printf("ctrl: ERROR %s: %v %v", msg, err, kv)
}
func (l *stdLogr) WithValues(kv ...any) logr.LogSink { return l }
func (l *stdLogr) WithName(name string) logr.LogSink {
	return &stdLogr{name: name}
}

// stdLogger returns a logr.Logger writing to the standard logger.
func stdLogger() logr.Logger {
	return logr.New(&stdLogr{})
}
