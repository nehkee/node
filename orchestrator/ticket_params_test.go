package orchestrator

import (
	"errors"
	"math/big"
	"testing"

	"github.com/livepeer/node/pm"
	"github.com/stretchr/testify/require"
)

func bigInt(t *testing.T, value string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(value, 10)
	require.True(t, ok, value)
	return n
}

func TestPayerTicketParams(t *testing.T) {
	// 1-in-1000 win chance, expressed as a numerator over 2^256-1.
	oneIn1000 := new(big.Int).Quo(maxWinProb, big.NewInt(1000))
	faceValue := bigInt(t, "2000000000000000000")
	minFaceValue := bigInt(t, "100000000000000000")
	targetEV := new(big.Rat).Mul(new(big.Rat).SetInt(faceValue), new(big.Rat).SetFrac(oneIn1000, maxWinProb))
	for _, test := range []struct {
		name  string
		float *big.Int
		err   error
	}{
		{"sufficient float", new(big.Int).Mul(faceValue, big.NewInt(10)), nil},
		{"exact float", faceValue, nil},
		{"half float", new(big.Int).Quo(faceValue, big.NewInt(2)), nil},
		{"uneven float", bigInt(t, "1333333333333333337"), nil},
		{"minimum float", minFaceValue, nil},
		{"below minimum", new(big.Int).Sub(minFaceValue, big.NewInt(1)), errPayerFloatTooLow},
		{"empty", new(big.Int), errPayerFloatTooLow},
	} {
		t.Run(test.name, func(t *testing.T) {
			fv, wp, err := payerTicketParams(faceValue, oneIn1000, minFaceValue, test.float)
			if test.err != nil {
				require.ErrorIs(t, err, test.err)
				require.ErrorIs(t, err, pm.ErrPayerUnavailable)
				return
			}
			require.NoError(t, err)
			if test.float.Cmp(faceValue) >= 0 {
				require.Equal(t, faceValue, fv, "sufficient payers keep the configured pair")
				require.Equal(t, oneIn1000, wp)
				return
			}
			require.Equal(t, test.float, fv, "the face value is redeemable from the float")
			require.Negative(t, wp.Cmp(maxWinProb))
			ev := new(big.Rat).Mul(new(big.Rat).SetInt(fv), new(big.Rat).SetFrac(wp, maxWinProb))
			require.GreaterOrEqual(t, ev.Cmp(targetEV), 0, "EV never falls below the target")
			// The excess is below one unit of win probability at this face value.
			oneUnit := new(big.Rat).SetFrac(fv, maxWinProb)
			require.Negative(t, new(big.Rat).Sub(ev, targetEV).Cmp(oneUnit))
		})
	}

	t.Run("issue example", func(t *testing.T) {
		// A $2 face value at 1-in-1000 becomes $1 at about 1-in-500.
		fv, wp, err := payerTicketParams(faceValue, oneIn1000, minFaceValue, new(big.Int).Quo(faceValue, big.NewInt(2)))
		require.NoError(t, err)
		require.Equal(t, new(big.Int).Quo(faceValue, big.NewInt(2)), fv)
		odds := new(big.Rat).SetFrac(maxWinProb, wp)
		require.Equal(t, "500.0", odds.FloatString(1))
	})

	t.Run("always winning ticket is rejected", func(t *testing.T) {
		// EV is just below the face value, so halving the face value would
		// need a win probability above one.
		almostCertain := new(big.Int).Sub(maxWinProb, big.NewInt(1))
		_, _, err := payerTicketParams(big.NewInt(10), almostCertain, big.NewInt(1), big.NewInt(5))
		require.ErrorIs(t, err, errPayerFloatTooLow)
	})

	t.Run("invalid configuration", func(t *testing.T) {
		for _, args := range [][4]*big.Int{
			{nil, oneIn1000, minFaceValue, faceValue},
			{faceValue, new(big.Int), minFaceValue, faceValue},
			{faceValue, maxWinProb, minFaceValue, faceValue},
			{faceValue, oneIn1000, new(big.Int), faceValue},
			{faceValue, oneIn1000, new(big.Int).Add(faceValue, big.NewInt(1)), faceValue},
		} {
			_, _, err := payerTicketParams(args[0], args[1], args[2], args[3])
			require.Error(t, err)
			require.False(t, errors.Is(err, pm.ErrPayerUnavailable))
		}
	})
}

func TestPayerFloat(t *testing.T) {
	funds := func(deposit, reserve int64) pm.PayerFunds {
		return pm.PayerFunds{Snapshot: pm.ChainSnapshot{Round: big.NewInt(5)}, Deposit: big.NewInt(deposit), Reserve: big.NewInt(reserve), WithdrawRound: new(big.Int)}
	}
	for _, test := range []struct {
		name                        string
		deposit, reserve, liability int64
		want                        int64
	}{
		{"reserve limited", 1000, 100, 0, 100},
		{"deposit limited", 50, 100, 0, 50},
		{"outstanding tickets reduce float", 1000, 100, 30, 70},
		{"liability exceeds reserve", 1000, 100, 150, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			float, err := payerFloat(funds(test.deposit, test.reserve), big.NewInt(test.liability))
			require.NoError(t, err)
			require.Zero(t, big.NewInt(test.want).Cmp(float), float.String())
		})
	}
	_, err := payerFloat(funds(0, 100), new(big.Int))
	require.ErrorIs(t, err, pm.ErrPayerUnavailable, "an empty deposit is unavailable")
	_, err = payerFloat(funds(100, 100), big.NewInt(-1))
	require.Error(t, err)
}
