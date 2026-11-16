package orchestrator

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/livepeer/node/pm"
)

// errPayerFloatTooLow reports that a payer cannot back a winning ticket of at
// least the configured minimum face value. It wraps pm.ErrPayerUnavailable so
// callers and metrics classify it like the receipt-time funds rejection.
var errPayerFloatTooLow = fmt.Errorf("%w: available payer float is below the minimum ticket face value", pm.ErrPayerUnavailable)

// payerFloat is the amount a payer can still guarantee to this recipient for a
// new winning ticket: the claimable reserve, less the face value of this
// payer's winning tickets that are stored but not yet settled, capped by the
// deposit. It mirrors go-livepeer's MaxFloat (reserve allocation less pending
// redemptions); the deposit cap preserves the receipt-time check that the
// deposit alone covers one ticket.
func payerFloat(funds pm.PayerFunds, liability *big.Int) (*big.Int, error) {
	if err := pm.ValidatePayerFunds(funds); err != nil {
		return nil, fmt.Errorf("%w: %v", pm.ErrPayerUnavailable, err)
	}
	if liability == nil || liability.Sign() < 0 {
		return nil, errors.New("invalid unsettled payer liability")
	}
	float := new(big.Int).Sub(funds.Reserve, liability)
	if float.Cmp(funds.Deposit) > 0 {
		float.Set(funds.Deposit)
	}
	if float.Sign() < 0 {
		float.SetInt64(0)
	}
	return float, nil
}

// payerTicketParams chooses the face value and win probability offered to one
// payer. The configured face value and win probability define the target
// expected value; the configured face value is also the maximum.
//
// A payer whose float covers the configured face value receives exactly the
// configured pair, as before. Otherwise the face value drops to the float, and
// the win probability rises so the ticket EV is at least the target and exceeds
// it by less than one unit of win probability (integer rounding favors the
// recipient). A face value below minFaceValue, or one so small that the
// ticket would always win, is rejected rather than advertised.
func payerTicketParams(faceValue, winProb, minFaceValue, float *big.Int) (*big.Int, *big.Int, error) {
	if faceValue == nil || winProb == nil || minFaceValue == nil || float == nil || faceValue.Sign() <= 0 || winProb.Sign() <= 0 || winProb.Cmp(maxWinProb) >= 0 || minFaceValue.Sign() <= 0 || minFaceValue.Cmp(faceValue) > 0 {
		return nil, nil, errors.New("invalid ticket parameter configuration")
	}
	if float.Cmp(faceValue) >= 0 {
		return new(big.Int).Set(faceValue), new(big.Int).Set(winProb), nil
	}
	if float.Cmp(minFaceValue) < 0 {
		return nil, nil, errPayerFloatTooLow
	}
	// winProb' = ceil(winProb * faceValue / float), so float * winProb' covers
	// the target EV numerator faceValue * winProb.
	target := new(big.Int).Mul(winProb, faceValue)
	scaled, remainder := new(big.Int).QuoRem(target, float, new(big.Int))
	if remainder.Sign() != 0 {
		scaled.Add(scaled, big.NewInt(1))
	}
	if scaled.Cmp(maxWinProb) >= 0 {
		return nil, nil, errPayerFloatTooLow
	}
	return new(big.Int).Set(float), scaled, nil
}
