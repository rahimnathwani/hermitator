package gateway

import (
	"slices"
	"testing"

	"go.mau.fi/whatsmeow"
)

func TestChosenOptions(t *testing.T) {
	opts := []string{"Yes", "No", "Staging first"}
	vote := whatsmeow.HashPollOptions([]string{"Staging first"})
	if got := chosenOptions(opts, vote); !slices.Equal(got, []string{"Staging first"}) {
		t.Fatalf("chosen = %q", got)
	}
	if got := chosenOptions(opts, whatsmeow.HashPollOptions([]string{"Maybe"})); len(got) != 0 {
		t.Fatalf("an option the poll never had was chosen: %q", got)
	}
}
