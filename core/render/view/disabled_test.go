// SPDX-License-Identifier: AGPL-3.0-or-later

package view

import (
	"context"
	"testing"

	pluginapi "github.com/johnnybravo-xyz/suchi/plugin-api"
)

func TestDisabledHandlerCompletesRender(t *testing.T) {
	h := NewDisabledHandler()
	if got := h.Kinds(); len(got) != 1 || got[0] != Kind {
		t.Fatalf("kinds = %v", got)
	}
	if err := h.Handle(context.Background(), pluginapi.Event{DocID: 42}); err != nil {
		t.Fatalf("disabled rendering must complete queued work: %v", err)
	}
}
