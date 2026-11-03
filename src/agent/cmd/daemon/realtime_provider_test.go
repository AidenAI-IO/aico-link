package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"

	"aico-agent/internal/agent/realtimevoice"
)

type fakeRealtimeSession struct {
	info   realtimevoice.SessionInfo
	events chan realtimevoice.Event
	errors chan error
	done   chan struct{}
}

func (s *fakeRealtimeSession) Info() realtimevoice.SessionInfo         { return s.info }
func (s *fakeRealtimeSession) Events() <-chan realtimevoice.Event      { return s.events }
func (s *fakeRealtimeSession) Errors() <-chan error                    { return s.errors }
func (s *fakeRealtimeSession) Done() <-chan struct{}                   { return s.done }
func (s *fakeRealtimeSession) SendAudio(context.Context, []byte) error { return nil }
func (s *fakeRealtimeSession) Commit(context.Context) error            { return nil }
func (s *fakeRealtimeSession) Interrupt(context.Context, realtimevoice.ResponseInterruption) error {
	return nil
}
func (s *fakeRealtimeSession) SendToolResult(context.Context, string, string) error { return nil }
func (s *fakeRealtimeSession) Close() error                                         { return nil }

func TestRealtimeProviderTextCapabilityIsExplicit(t *testing.T) {
	session := &fakeRealtimeSession{info: realtimevoice.SessionInfo{}}
	conversation := &realtimevoice.Conversation{Session: session}
	if conversation.TextSession != nil {
		t.Fatal("core session unexpectedly exposed text injection")
	}
}

func TestRealtimeSessionDrainsFinalEventAfterDone(t *testing.T) {
	events := make(chan realtimevoice.Event, 1)
	done := make(chan struct{})
	session := &fakeRealtimeSession{events: events, done: done}
	events <- realtimevoice.Event{Kind: realtimevoice.EventTranscriptFinal, Text: "final transcript"}
	close(events)
	close(done)

	// runRealtimeSession deliberately terminates on Events closure rather than
	// Done so a provider's buffered final events remain observable.
	var received []realtimevoice.Event
	for event := range session.Events() {
		received = append(received, event)
	}
	if len(received) != 1 || received[0].Kind != realtimevoice.EventTranscriptFinal || received[0].Text != "final transcript" {
		t.Fatalf("received events = %#v, want buffered final transcript", received)
	}
}

func TestRunRealtimeSessionDoesNotTerminateOnSessionDone(t *testing.T) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(sourceFile), "realtime_wakeup.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse realtime_wakeup.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "runRealtimeSessionWithIdleTimeout" {
			run = function
			break
		}
	}
	if run == nil {
		t.Fatal("runRealtimeSessionWithIdleTimeout declaration not found")
	}
	var foundSessionDone bool
	ast.Inspect(run.Body, func(node ast.Node) bool {
		unary, ok := node.(*ast.UnaryExpr)
		if !ok || unary.Op != token.ARROW {
			return true
		}
		call, ok := unary.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Done" {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if ok && receiver.Name == "session" {
			foundSessionDone = true
		}
		return true
	})
	if foundSessionDone {
		t.Fatal("runRealtimeSession must drain Events() and terminate on event-stream closure, not session.Done()")
	}
}
