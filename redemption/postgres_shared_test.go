package redemption

import "testing"

// sharedStoreTests exercise the complete store lifecycle through helpers that
// honor testBackend. They run on SQLite as ordinary tests; this list re-runs
// them on PostgreSQL. Tests that inspect SQLite files, legacy SQLite fixtures
// or SQLite migration history stay SQLite-only.
var sharedStoreTests = map[string]func(*testing.T){
	"SQLiteWinningTicketLifecycle":                         TestSQLiteWinningTicketLifecycle,
	"NonTransactionSettlementStatus":                       TestNonTransactionSettlementStatus,
	"ReceiptSweepRevisitsHistoryDuringAppends":             TestReceiptSweepRevisitsHistoryDuringAppends,
	"HTTPAdmissionIdentityCapacityAndScope":                TestHTTPAdmissionIdentityCapacityAndScope,
	"HTTPAuthorizationConcurrency":                         TestHTTPAuthorizationConcurrency,
	"RemoteDeliveryLostAcknowledgmentAndKeylessSettlement": TestRemoteDeliveryLostAcknowledgmentAndKeylessSettlement,
	"BoundQueueMetricStates":                               TestBoundQueueMetricStates,
	"RedemptionMetricsDurableAndIdempotent":                TestRedemptionMetricsDurableAndIdempotent,
	"DroppedRedemptionRefillsNonceAfterRestart":            TestDroppedRedemptionRefillsNonceAfterRestart,
	"ClosedTicketRepairsSupersededHigherNonce":             TestClosedTicketRepairsSupersededHigherNonce,
	"DestinationChangeWaitsForNonceDrain":                  TestDestinationChangeWaitsForNonceDrain,
	"FinalizedExternalNonceRetiresOnlyMissingTransactions": TestFinalizedExternalNonceRetiresOnlyMissingTransactions,
	"FinalizedExternalNoncePreservesHigherAttempt":         TestFinalizedExternalNoncePreservesHigherAttempt,
	"FinalizedNonceAdvancesPastResolvedHistory":            TestFinalizedNonceAdvancesPastResolvedHistory,
	"SharedWalletRedemptionRecovery":                       TestSharedWalletRedemptionRecovery,
	"RejectedRedemptionDoesNotSkipNonce":                   TestRejectedRedemptionDoesNotSkipNonce,
	"RedemptionReplacementAndFinality":                     TestRedemptionReplacementAndFinality,
	"RedemptionReplacementFailuresRemainQueued":            TestRedemptionReplacementFailuresRemainQueued,
	"LostResponseUsesFinalizedTicketGuard":                 TestLostResponseUsesFinalizedTicketGuard,
	"UsedTicketGuardRecoversAfterReorg":                    TestUsedTicketGuardRecoversAfterReorg,
	"RedemptionRestartRestoresTransactionCursor":           TestRedemptionRestartRestoresTransactionCursor,
	"RedemptionRecoveryBeforeAndAfterBroadcast":            TestRedemptionRecoveryBeforeAndAfterBroadcast,
	"TerminalAttemptRetainsUnresolvedSibling":              TestTerminalAttemptRetainsUnresolvedSibling,
	"OrphanedInclusionRecoversSameNonce":                   TestOrphanedInclusionRecoversSameNonce,
	"RedemptionPersistencePreventsInitialSend":             TestRedemptionPersistencePreventsInitialSend,
	"ManualRedemptionRetryCannotDisplaceActiveIdentity":    TestManualRedemptionRetryCannotDisplaceActiveIdentity,
	"ExpiredAttemptDoesNotReopenRandomness":                TestExpiredAttemptDoesNotReopenRandomness,
	"MinedFailureOutlivesInclusionDeadline":                TestMinedFailureOutlivesInclusionDeadline,
	"TerminalTicketKeepsUnresolvedReceipts":                TestTerminalTicketKeepsUnresolvedReceipts,
	"RevertedReplacementReleasesSameNonceLiability":        TestRevertedReplacementReleasesSameNonceLiability,
	"ManualRetryFailureKeepsActiveMonitor":                 TestManualRetryFailureKeepsActiveMonitor,
	"ConsumedNonceResolvesOtherTicketHistory":              TestConsumedNonceResolvesOtherTicketHistory,
	"MultipleFinalizedFailuresResolveEveryGroup":           TestMultipleFinalizedFailuresResolveEveryGroup,
	"LiveAcknowledgementRestoresNonce":                     TestLiveAcknowledgementRestoresNonce,
	"UsedTransitionRecoversAfterStorageFailure":            TestUsedTransitionRecoversAfterStorageFailure,
	"RejectedReplacementKeepsPredecessorReconcilable":      TestRejectedReplacementKeepsPredecessorReconcilable,
	"FinalizedUseSettlesUnresolvedAttempts":                TestFinalizedUseSettlesUnresolvedAttempts,
	"RedemptionCallbackObservations":                       TestRedemptionCallbackObservations,
	"CollateralWatermarkSurvivesSettlementAndRestart":      TestCollateralWatermarkSurvivesSettlementAndRestart,
	"RedemptionRequiresBoundDeployment":                    TestRedemptionRequiresBoundDeployment,
	"CanceledRedemptionPassRotatesUnvisitedTickets":        TestCanceledRedemptionPassRotatesUnvisitedTickets,
	"DeliveryOutcomeSurvivesPassCancellation":              TestDeliveryOutcomeSurvivesPassCancellation,
	"QueuePageRotatesAcrossRecipients":                     TestQueuePageRotatesAcrossRecipients,
	"BoundQueueFairnessLiabilityAndFinalizedExpiry":        TestBoundQueueFairnessLiabilityAndFinalizedExpiry,
	"StandaloneCostAndWalletPoliciesCoverSavedBytes":       TestStandaloneCostAndWalletPoliciesCoverSavedBytes,
	"WithheldReplacementPreservesPendingTicket":            TestWithheldReplacementPreservesPendingTicket,
	"RedemptionEligibility":                                TestRedemptionEligibility,
	"TerminalRedemptionSubmissionError":                    TestTerminalRedemptionSubmissionError,
}

func TestPostgresSharedStoreBehavior(t *testing.T) {
	postgresTestURL(t)
	previous := testBackend
	testBackend = BackendPostgres
	t.Cleanup(func() { testBackend = previous })
	for name, test := range sharedStoreTests {
		t.Run(name, test)
	}
}
