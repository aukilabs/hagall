package websocket

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aukilabs/go-tooling/pkg/logs"
	"github.com/stretchr/testify/require"
)

func TestHandlerWithLogsIncCounter(t *testing.T) {
	h := HandlerWithLogs(&RealtimeHandler{}, time.Second).(*handlerWithLogs)
	defer h.Close()

	h.incCounter("test")
	require.Equal(t, 1, h.counter["test"])
}

func TestHandlerWithLogsLogSummary(t *testing.T) {
	testClientID := "test-client"
	h := HandlerWithLogs(&RealtimeHandler{clientID: testClientID}, time.Second).(*handlerWithLogs)
	defer h.Close()

	h.incCounter("test-1")
	h.incCounter("test-1")
	h.incCounter("test-2")

	var b strings.Builder
	logs.SetInlineEncoder()
	logs.SetLogger(func(e logs.Entry) {
		fmt.Fprint(&b, e)
	})

	h.logSummary()
	require.Empty(t, h.counter)

	logString := b.String()
	clientIDTag := fmt.Sprintf(`"%s":"%s"`, logs.ClientIDTag, testClientID)
	require.Contains(t, logString, `"test-1":2`)
	require.Contains(t, logString, `"test-2":1`)
	require.Contains(t, logString, clientIDTag)
	t.Log(b.String())
}

func TestHandlerWithLogsStartSummaryWorker(t *testing.T) {
	var wg sync.WaitGroup
	var once sync.Once

	var b strings.Builder
	logs.SetInlineEncoder()
	logs.SetLogger(func(e logs.Entry) {
		fmt.Fprint(&b, e)
		once.Do(wg.Done)
	})

	wg.Add(1)
	h := HandlerWithLogs(&RealtimeHandler{}, time.Millisecond).(*handlerWithLogs)
	defer h.Close()

	// This is to avoid the test block since no summary is sent if no counter is
	// incremented.
	h.incCounter("test-1")

	wg.Wait()
	out := b.String()
	require.NotEmpty(t, out)
	t.Log(out)
}

func TestWillFail(t *testing.T) {
	t.Log("this test always fail")
	t.Fail()
}
