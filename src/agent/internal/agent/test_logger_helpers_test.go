package agent

import (
	"io"
	"log"

	"aico-agent/internal/agent/statemanager"
)

func newTestLogger() *Logger {
	return &Logger{logger: log.New(io.Discard, "", 0)}
}

func newServerForTest(runtime *Runtime) *Server {
	if runtime.logger == nil {
		runtime.logger = newTestLogger()
	}
	if runtime.stateManager == nil {
		runtime.stateManager = statemanager.NewStateManager()
	}
	if runtime.phoneBridge == nil {
		runtime.phoneBridge = NewPhoneBridge(runtime.logger)
		runtime.stateManager.RegisterUpdater(runtime.phoneBridge)
	}
	return NewServer(runtime, ":0")
}
