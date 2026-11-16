package orchestrator

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/pm/wire"
	"github.com/stretchr/testify/require"
)

// newPayerParamsEngine configures a face value of 1000 wei at a 1-in-2 win
// probability (EV 500 wei), so reduced face values stay below certainty.
func newPayerParamsEngine(t *testing.T, min int64) (*PaymentEngine, *recipientTestChain, *ecdsa.PrivateKey, *int) {
	t.Helper()
	store, err := OpenRedeemerDB(filepath.Join(t.TempDir(), "payments.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	chain := &recipientTestChain{active: true, block: 50}
	e, err := NewPaymentEngine(store, chain, ethcommon.HexToAddress("0x1234"), big.NewInt(1000), new(big.Int).Rsh(maxWinProb, 1))
	require.NoError(t, err)
	if min > 0 {
		require.NoError(t, e.SetMinFaceValue(big.NewInt(min)))
	}
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	reads := new(int)
	return e, chain, key, reads
}

func payerReserve(reads *int, reserve func() int64) func(context.Context, pm.PayerFunds) (pm.PayerFunds, error) {
	return func(_ context.Context, funds pm.PayerFunds) (pm.PayerFunds, error) {
		*reads++
		funds.Reserve.SetInt64(reserve())
		return funds, nil
	}
}

func challengeParams(t *testing.T, info wire.OrchestratorInfo) (*big.Int, *big.Int) {
	t.Helper()
	return new(big.Int).SetBytes(info.TicketParams.FaceValue), new(big.Int).SetBytes(info.TicketParams.WinProb)
}

func TestPayerSpecificTicketParams(t *testing.T) {
	configuredWinProb := new(big.Int).Rsh(maxWinProb, 1)
	targetEV := new(big.Rat).Mul(big.NewRat(1000, 1), new(big.Rat).SetFrac(configuredWinProb, maxWinProb))

	t.Run("disabled keeps fixed parameters without reading funds", func(t *testing.T) {
		e, chain, key, reads := newPayerParamsEngine(t, 0)
		chain.funds = payerReserve(reads, func() int64 { return 1 })
		fv, wp := challengeParams(t, issueRecipientParams(t, e, key))
		require.Zero(t, *reads)
		require.Zero(t, fv.Cmp(big.NewInt(1000)))
		require.Zero(t, wp.Cmp(configuredWinProb))
	})

	t.Run("sufficient float keeps fixed parameters", func(t *testing.T) {
		e, chain, key, reads := newPayerParamsEngine(t, 600)
		chain.funds = payerReserve(reads, func() int64 { return 1_000_000 })
		fv, wp := challengeParams(t, issueRecipientParams(t, e, key))
		require.Equal(t, 1, *reads)
		require.Zero(t, fv.Cmp(big.NewInt(1000)))
		require.Zero(t, wp.Cmp(configuredWinProb))
	})

	t.Run("reduced float lowers face value at the same EV and is redeemable", func(t *testing.T) {
		e, chain, key, reads := newPayerParamsEngine(t, 600)
		chain.funds = payerReserve(reads, func() int64 { return 800 })
		info := issueRecipientParams(t, e, key)
		fv, wp := challengeParams(t, info)
		require.Zero(t, fv.Cmp(big.NewInt(800)))
		ev := new(big.Rat).Mul(new(big.Rat).SetInt(fv), new(big.Rat).SetFrac(wp, maxWinProb))
		require.GreaterOrEqual(t, ev.Cmp(targetEV), 0)
		require.Negative(t, new(big.Rat).Sub(ev, targetEV).Cmp(new(big.Rat).SetFrac(fv, maxWinProb)))
		// The receipt-time funds check accepts the advertised face value.
		require.NoError(t, receiveRecipientPayment(t, e, key, info, 1))
	})

	t.Run("outstanding winning tickets reduce the float", func(t *testing.T) {
		e, chain, key, reads := newPayerParamsEngine(t, 600)
		chain.funds = payerReserve(reads, func() int64 { return 1000 })
		payer := crypto.PubkeyToAddress(key.PublicKey)
		require.NoError(t, e.store.StoreWinningTicket(&pm.SignedTicket{Ticket: &pm.Ticket{PayerAddress: payer, Recipient: e.recipient, FaceValue: big.NewInt(300), WinProb: big.NewInt(1), TicketNonce: 1, RecipientRandHash: crypto.Keccak256Hash([]byte("outstanding")), CreationRound: 5, ParamsExpirationBlock: big.NewInt(60)}, Sig: make([]byte, 65), RecipientRand: big.NewInt(1)}))
		fv, _ := challengeParams(t, issueRecipientParams(t, e, key))
		require.Zero(t, fv.Cmp(big.NewInt(700)), fv.String())
	})

	t.Run("insufficient float is rejected", func(t *testing.T) {
		e, chain, key, reads := newPayerParamsEngine(t, 600)
		chain.funds = payerReserve(reads, func() int64 { return 599 })
		_, err := e.MakeChallenge(t.Context(), "runner", "session", crypto.PubkeyToAddress(key.PublicKey), 9, "fixed", "https://orch.example")
		require.ErrorIs(t, err, errPayerFloatTooLow)
		require.ErrorIs(t, err, pm.ErrPayerUnavailable)
		require.EqualValues(t, 0, len(e.sessions), "no challenge is advertised")
	})

	t.Run("RPC failure is not a funds rejection", func(t *testing.T) {
		e, chain, key, _ := newPayerParamsEngine(t, 600)
		chain.funds = func(context.Context, pm.PayerFunds) (pm.PayerFunds, error) {
			return pm.PayerFunds{}, errors.New("RPC unavailable")
		}
		_, err := e.MakeChallenge(t.Context(), "runner", "session", crypto.PubkeyToAddress(key.PublicKey), 9, "fixed", "https://orch.example")
		require.Error(t, err)
		require.False(t, errors.Is(err, pm.ErrPayerUnavailable))
		require.Equal(t, "error", paymentOutcome(err))
	})

	t.Run("funds falling after the challenge are rejected at receipt", func(t *testing.T) {
		e, chain, key, reads := newPayerParamsEngine(t, 600)
		reserve := int64(800)
		chain.funds = payerReserve(reads, func() int64 { return reserve })
		info := issueRecipientParams(t, e, key)
		reserve = 700
		require.ErrorIs(t, receiveRecipientPayment(t, e, key, info, 1), pm.ErrPayerUnavailable)
		// A refreshed challenge recomputes the face value from current funds.
		challenge, err := e.RefreshChallenge(t.Context(), "session", crypto.PubkeyToAddress(key.PublicKey), "https://orch.example")
		require.NoError(t, err)
		fv, _ := challengeParams(t, decodeChallenge(t, challenge))
		require.Zero(t, fv.Cmp(big.NewInt(700)))
	})

	t.Run("challenge HTTP status", func(t *testing.T) {
		e, chain, key, reads := newPayerParamsEngine(t, 600)
		chain.funds = payerReserve(reads, func() int64 { return 1 })
		registry, server := reviewServer(t, "http://127.0.0.1:1")
		defer server.Close()
		registry.SetWeiPerUSD(big.NewRat(1, 1))
		require.NoError(t, registry.AddStatic(StaticRunner{ID: "runner", RunnerURL: "http://127.0.0.1:1", App: "test", Mode: "persistent", Capacity: 1, PriceInfo: priceInfo{Price: "9", Currency: "usd", Unit: "fixed"}}))
		server.SetPayment(e)
		request := httptest.NewRequest(http.MethodPost, "/apps/runner/session", nil)
		request.Host = server.apiOrigin.Host
		request.Header.Set("Livepeer-Payer-Address", crypto.PubkeyToAddress(key.PublicKey).Hex())
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), errPayerFundsMessage)
	})
}

func decodeChallenge(t *testing.T, challenge Challenge) wire.OrchestratorInfo {
	t.Helper()
	encoded, err := base64.StdEncoding.DecodeString(challenge.PaymentParams)
	require.NoError(t, err)
	info, err := wire.DecodeOrchestratorInfo(encoded)
	require.NoError(t, err)
	return info
}
