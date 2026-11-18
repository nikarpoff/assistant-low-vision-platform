package textnorm

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// casesPath is the table of the normalization cases shared with the ASR service.
const casesPath = "../../testdata/contracts/asr-service/normalize.cases.json"

// normalizeCase is one case of the table.
type normalizeCase struct {
	Name     string `json:"name"`
	Input    string `json:"input"`
	Expected string `json:"expected"`
}

// readCases reads the table of the normalization cases.
func readCases(t *testing.T) []normalizeCase {
	t.Helper()
	body, err := os.ReadFile(casesPath)
	require.NoError(t, err)
	table := struct {
		Cases []normalizeCase `json:"cases"`
	}{}
	require.NoError(t, json.Unmarshal(body, &table))
	return table.Cases
}

func TestNormalize(t *testing.T) {
	var (
		cases []normalizeCase
	)

	setup := func(t *testing.T) {
		cases = readCases(t)
	}

	t.Run("must_pass_contract_cases", func(t *testing.T) {
		setup(t)
		require.GreaterOrEqual(t, len(cases), 20)

		for _, tc := range cases {
			t.Run(fmt.Sprintf("must_pass_case_%s", tc.Name), func(t *testing.T) {
				normalized := Normalize(tc.Input)

				require.Equal(t, tc.Expected, normalized)
			})
		}
	})

	t.Run("must_pass_symbols_and_digits_kept", func(t *testing.T) {
		setup(t)

		normalized := Normalize("Ступени: 2+3 = 5 $")

		require.Equal(t, "ступени 2+3 = 5 $", normalized)
	})

	t.Run("must_pass_idempotent", func(t *testing.T) {
		setup(t)

		once := Normalize("  Ещё раз, ПОЖАЛУЙСТА!  ")

		require.Equal(t, "еще раз пожалуйста", once)
		require.Equal(t, once, Normalize(once))
	})
}
