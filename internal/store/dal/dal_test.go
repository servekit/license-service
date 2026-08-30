package dal_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/stretchr/testify/require"

	"github.com/servekit/go-common/dbx"

	"github.com/servekit/license-service/internal/store/dal"
	"github.com/servekit/license-service/internal/store/models"
)

// fakeHash returns a deterministic 64-hex string seeded by n (the shape of
// real SHA-256 key hashes).
func fakeHash(n byte) string {
	return fmt.Sprintf("%064d", n)
}

func fakeFingerprint(n byte) string {
	return "v1." + fakeHash(n)
}

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, dbx.AutoMigrate(db, models.AllModels()...))
	return db
}

func mkKey(t *testing.T, db *gorm.DB, hash string, slots int32) *models.LicenseKey {
	t.Helper()
	require.Len(t, hash, 64, "key_hash must look like a 64-hex SHA-256")
	k := &models.LicenseKey{
		KeyHash:   hash,
		LicenseID: "lk_" + hash[:32],
		KeyPrefix: "AV1D-" + hash[:3], // 8 chars, matches the column size
		Label:     "test",
		MaxSlots:  slots,
		Status:    1,
	}
	require.NoError(t, dal.CreateKey(context.Background(), db, k))
	return k
}

func mkDevice(token, fp string) *models.LicenseDevice {
	return &models.LicenseDevice{
		DeviceToken:   token,
		FingerprintID: fp,
		LastSeenAt:    time.Now(),
	}
}

func tokenFor(i int) string {
	return fmt.Sprintf("%08d-0000-4000-8000-%012d", i, i)
}

// TestKey_CRUDAndDualFormID: create/fetch by hash, lk_-prefixed license id,
// and raw hash all resolve; status flips carry revoked_at.
func TestKey_CRUDAndDualFormID(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	hash := fakeHash(1)
	k := mkKey(t, db, hash, 3)

	got, err := dal.GetKeyByKeyHash(ctx, db, hash)
	require.NoError(t, err)
	require.Equal(t, k.LicenseID, got.LicenseID)

	byID, err := dal.GetKeyByIDOrHash(ctx, db, k.LicenseID)
	require.NoError(t, err)
	require.Equal(t, hash, byID.KeyHash)

	byHash, err := dal.GetKeyByIDOrHash(ctx, db, hash)
	require.NoError(t, err)
	require.Equal(t, k.LicenseID, byHash.LicenseID)

	// Revoke sets the timestamp; unrevoke clears it back to NULL.
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, dal.SetKeyStatus(ctx, db, hash, 2, &now))
	revoked, err := dal.GetKeyByKeyHash(ctx, db, hash)
	require.NoError(t, err)
	require.NotNil(t, revoked.RevokedAt)
	require.NoError(t, dal.SetKeyStatus(ctx, db, hash, 1, nil))
	active, err := dal.GetKeyByKeyHash(ctx, db, hash)
	require.NoError(t, err)
	require.Nil(t, active.RevokedAt)

	// ListKeys filters by status (0 = all).
	all, err := dal.ListKeys(ctx, db, 0, 10)
	require.NoError(t, err)
	require.Len(t, all, 1)
}

// TestEntitlement_UpsertIncludingNullClear: upsert replaces kind/expiry and
// can switch a module back to perpetual (expires_at NULL).
func TestEntitlement_UpsertIncludingNullClear(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	hash := fakeHash(2)
	mkKey(t, db, hash, 3)

	exp := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, dal.UpsertEntitlement(ctx, db, &models.LicenseEntitlement{
		KeyHash: hash, Module: 1, Kind: 2, ExpiresAt: &exp, GrantedAt: exp,
	}))
	require.NoError(t, dal.UpsertEntitlement(ctx, db, &models.LicenseEntitlement{
		KeyHash: hash, Module: 1, Kind: 1, ExpiresAt: nil, GrantedAt: exp,
	}))
	ents, err := dal.ListEntitlementsByKeyHash(ctx, db, hash)
	require.NoError(t, err)
	require.Len(t, ents, 1)
	require.EqualValues(t, 1, ents[0].Kind)
	require.Nil(t, ents[0].ExpiresAt)

	require.NoError(t, dal.DeleteEntitlement(ctx, db, hash, 1))
	ents, err = dal.ListEntitlementsByKeyHash(ctx, db, hash)
	require.NoError(t, err)
	require.Empty(t, ents)
}

// TestDevice_SlotLifecycle: count/list/insert/delete and the unique index
// blocking a same-token double insert.
func TestDevice_SlotLifecycle(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	hash := fakeHash(3)

	d1 := mkDevice(tokenFor(1), fakeFingerprint(1))
	d1.KeyHash = hash
	require.NoError(t, dal.InsertDevice(ctx, db, d1))

	d2 := mkDevice(tokenFor(2), fakeFingerprint(2))
	d2.KeyHash = hash
	require.NoError(t, dal.InsertDevice(ctx, db, d2))

	n, err := dal.CountDevicesByKeyHash(ctx, db, hash)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)

	// Same token again violates the unique index.
	dup := mkDevice(tokenFor(1), fakeFingerprint(9))
	dup.KeyHash = hash
	require.Error(t, dal.InsertDevice(ctx, db, dup))

	// TouchDevice rebinds the fingerprint and stamps last_fingerprint_at.
	now := time.Now()
	require.NoError(t, dal.TouchDevice(ctx, db, hash, d1.DeviceToken, fakeFingerprint(3), now, true))
	got, err := dal.GetDevice(ctx, db, hash, d1.DeviceToken)
	require.NoError(t, err)
	require.Equal(t, fakeFingerprint(3), got.FingerprintID)
	require.NotNil(t, got.LastFingerprintAt)

	// DeleteDevice is idempotent (zero rows is fine).
	require.NoError(t, dal.DeleteDevice(ctx, db, hash, d2.DeviceToken))
	require.NoError(t, dal.DeleteDevice(ctx, db, hash, d2.DeviceToken))
	_, err = dal.GetDevice(ctx, db, hash, d2.DeviceToken)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// TestTrial_ConcurrentStartCollapsesToOneRow: N goroutines racing
// InsertOnConflictDoNothing leave exactly one row; every reader recovers the
// same started_at.
func TestTrial_ConcurrentStartCollapsesToOneRow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	fp := fakeFingerprint(4)
	const workers = 8

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = dal.InsertTrialOnConflictDoNothing(ctx, db, &models.LicenseTrial{
				FingerprintID:    fp,
				Module:           2,
				StartedAt:        time.Now(),
				FirstDeviceToken: tokenFor(i),
			})
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err, "ON CONFLICT DO NOTHING must not error")
	}

	trials, err := dal.ListTrialsByFingerprint(ctx, db, fp)
	require.NoError(t, err)
	require.Len(t, trials, 1)

	got, err := dal.GetTrial(ctx, db, fp, 2)
	require.NoError(t, err)
	require.Equal(t, trials[0].StartedAt, got.StartedAt)

	require.NoError(t, dal.DeleteTrial(ctx, db, fp, 2))
	_, err = dal.GetTrial(ctx, db, fp, 2)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// TestKeyForUpdate_SerializesSlotJudgment: two transactions racing the last
// slot must serialize — with FOR UPDATE on the key row, exactly one wins.
func TestKeyForUpdate_SerializesSlotJudgment(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	hash := fakeHash(5)
	mkKey(t, db, hash, 1) // one slot only

	tryOccupy := func(token string) error {
		return db.Transaction(func(tx *gorm.DB) error {
			k, err := dal.GetKeyForUpdate(ctx, tx, hash)
			if err != nil {
				return err
			}
			if _, err := dal.GetDevice(ctx, tx, hash, token); err == nil {
				return nil // already in slot
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			n, err := dal.CountDevicesByKeyHash(ctx, tx, hash)
			if err != nil {
				return err
			}
			if n >= int64(k.MaxSlots) {
				return errSlotLimit
			}
			d := mkDevice(token, fakeFingerprint(1))
			d.KeyHash = hash
			return dal.InsertDevice(ctx, tx, d)
		})
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = tryOccupy(tokenFor(i))
		}(i)
	}
	wg.Wait()

	var wins, conflicts int
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, errSlotLimit):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, wins, "exactly one racer occupies the last slot")
	require.Equal(t, 1, conflicts)

	n, err := dal.CountDevicesByKeyHash(ctx, db, hash)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

var errSlotLimit = errors.New("slot limit")
