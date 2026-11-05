package hook

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGraftReservedDenial checks what the reservations refuse an agent, by namespace and by kind of
// update, and that a denial does not name the reference.
func TestGraftReservedDenial(t *testing.T) {
	t.Parallel()

	oldValue, newValue, zero := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("0", 40)
	update := func(old, next, reference string) []byte { return []byte(old + " " + next + " " + reference + "\n") }

	for _, tc := range []struct {
		reference string
		updates   bool
		deletes   bool
	}{
		{reference: "refs/graft/green/main"},
		{reference: "refs/graft/proposals/x"},
		{reference: "refs/refs/heads/x", deletes: true},
		{reference: "refs/tags/refs/heads/x", deletes: true},
		{reference: "refs/heads/refs/heads/x", deletes: true},
		{reference: "refs/remotes/refs/heads/x", deletes: true},
		{reference: "refs/heads/x", updates: true, deletes: true},
		{reference: "refs/tags/x", updates: true, deletes: true},
		{reference: "refs/remotes/origin/x", updates: true, deletes: true},
		{reference: "refs/keep-around/x", updates: true, deletes: true},
		{reference: "HEAD", updates: true, deletes: true},
	} {
		t.Run(tc.reference, func(t *testing.T) {
			t.Parallel()

			for _, kind := range []struct {
				changes []byte
				allowed bool
			}{
				{update(zero, newValue, tc.reference), tc.updates},
				{update(oldValue, newValue, tc.reference), tc.updates},
				{update(oldValue, zero, tc.reference), tc.deletes},
				{update(zero, zero, tc.reference), tc.deletes},
			} {
				message, reference := graftReservedDenial(kind.changes)
				if kind.allowed {
					require.Empty(t, message, "%s", kind.changes)
					require.Empty(t, reference, "%s", kind.changes)
					continue
				}
				require.NotEmpty(t, message, "%s", kind.changes)
				require.Equal(t, tc.reference, reference, "the log has the name")
				require.NotContains(t, message, tc.reference, "the denial does not name it")
			}
		})
	}

	t.Run("any update of the transaction", func(t *testing.T) {
		t.Parallel()

		changes := append(update(oldValue, newValue, "refs/heads/x"), update(zero, newValue, "refs/tags/refs/heads/x")...)
		message, _ := graftReservedDenial(changes)
		require.NotEmpty(t, message)
	})
}
