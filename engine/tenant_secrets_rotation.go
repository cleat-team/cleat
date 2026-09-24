package engine

import (
	"context"
	"fmt"
	"sort"
)

// SecretKeyCheck is what the worker's startup check learns by comparing the
// table against the key ring.
type SecretKeyCheck struct {
	// Total is every secret row across every tenant, retired ones included.
	Total int
	// Unopenable maps a key_version this ring holds no key for to how many rows
	// carry it. Any entry here means a workflow that resolves one of those
	// secrets will fail at the plugin call.
	Unopenable map[int]int
	// Configured lists the key versions this ring holds, ascending, so a refusal
	// can say what IS configured beside what is missing.
	Configured []int
	// OnPrevious maps a version the ring can open, but which is not the current
	// key, to how many rows still carry it. Those rows work; they are what
	// reseal-secrets exists to move before the previous key is removed.
	OnPrevious map[int]int
}

// CheckKeyRing counts rows by key_version across every tenant and sorts each
// version into "this ring can open it", "this ring can open it but it is not
// current", and "this ring has no key for it".
//
// THE READ IS THE ONE CountSecrets USES, tenant by tenant with suspended tenants
// included, and for the reason cleat#2123 records: an unscoped read cannot see
// this table on PostgreSQL (it raises) or on SQL Server (it returns nothing).
// A version census that read nothing would report "every row is openable",
// which is exactly the wrong answer for the check whose whole purpose is to
// refuse a worker that cannot open a row (specs/CleatKeyRotation.tla, S1).
//
// A store with no ring reports every row as unopenable, which is true: it has
// no key for any of them.
func (s *SecretStore) CheckKeyRing(ctx context.Context) (SecretKeyCheck, error) {
	byVersion := map[int]int{}
	err := s.forEachTenant(ctx, func(tctx context.Context, tid string) error {
		return s.execTenantScoped(tctx, func(q querier) error {
			rows, err := q.QueryContext(tctx, keyVersionCountsStmt(s.dialect), tid)
			if err != nil {
				return fmt.Errorf("count secrets by key_version for tenant %s: %w", tid, err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var version, n int
				if err := rows.Scan(&version, &n); err != nil {
					return fmt.Errorf("scan key_version count for tenant %s: %w", tid, err)
				}
				byVersion[version] += n
			}
			return rows.Err()
		})
	})
	if err != nil {
		return SecretKeyCheck{}, err
	}
	chk := SecretKeyCheck{Unopenable: map[int]int{}, OnPrevious: map[int]int{}, Configured: s.ring.Versions()}
	for version, n := range byVersion {
		chk.Total += n
		switch {
		case s.ring == nil:
			chk.Unopenable[version] = n
		case version == s.ring.current.Version:
		default:
			if _, ok := s.ring.Key(version); ok {
				chk.OnPrevious[version] = n
			} else {
				chk.Unopenable[version] = n
			}
		}
	}
	return chk, nil
}

func keyVersionCountsStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT key_version, count(*) FROM tenant_secrets WHERE tenant_id = ? GROUP BY key_version`
	case "mssql":
		return `SELECT key_version, count(*) FROM tenant_secrets WHERE tenant_id = @p1 GROUP BY key_version`
	default:
		return `SELECT key_version, count(*) FROM tenant_secrets WHERE tenant_id = $1 GROUP BY key_version`
	}
}

// SecretReseal is the report from ResealSecrets. Every field exists because a
// sweep that says "done" without one is indistinguishable from a sweep that
// matched no rows.
type SecretReseal struct {
	Tenants int
	// Rows is every secret row seen, retired ones included.
	Rows int
	// Current is rows already sealed under the current key: nothing to do.
	Current int
	// Resealed is rows rewritten under the current key. In a dry run it is what
	// WOULD be rewritten, and nothing was.
	Resealed int
	// Changed is rows that were changed or removed between the read and the
	// write. Nothing was written for them, and a second run picks them up.
	Changed int
	// Unreadable is rows this ring cannot open: no key for their version, or a
	// key that does not decrypt them. Reported, never touched.
	Unreadable []UnreadableSecret
}

// UnreadableSecret names a row ResealSecrets could not open. It carries no
// plaintext and no key material: a tenant, a name, a version and the error.
type UnreadableSecret struct {
	TenantID   string
	Name       string
	KeyVersion int
	Reason     string
}

// Converged reports whether a second run would have nothing to do: nothing was
// left unreadable and nothing changed underneath the sweep.
func (r SecretReseal) Converged() bool { return len(r.Unreadable) == 0 && r.Changed == 0 }

// ResealSecrets re-encrypts every secret not already sealed under the current
// key, so that the previous key can be removed from the ring.
//
// Row by row and ONLINE. There is no lock and no window in which secrets do not
// resolve: each row keeps opening under whichever key its key_version names,
// right up to the statement that replaces it, and after it under the current
// key. That is safe only while every worker holds the current key, which is what
// the write gate in specs/CleatKeyRotation.tla is about and what the operator
// procedure in docs/how-to/use-secrets.md says to establish first.
//
// THE WRITE IS CONDITIONAL, AND THE CONDITION IS NOT OPTIONAL. It replaces a row
// only if that row still has the key_version AND the ciphertext this sweep read.
// Without that, a set-secret that lands between the read and the write is silently
// undone: the sweep writes back the OLD plaintext under the new key and reports
// success. The model finds it in eight states (ResealCAS = FALSE violates
// S2_NoLostWrite), and TestResealDoesNotOverwriteAConcurrentSetSecret is the
// Go form of that trace on all three dialects. A row that lost the race counts
// under Changed and is left for the next run, which is the right outcome: the
// newer value was written under the current key already.
//
// VERIFY BEFORE WRITING, as reseal-payloads does: the re-sealed value must open
// under the current key and round-trip to the plaintext read, or the sweep stops
// with an error before it has replaced anything it cannot restore. A re-seal
// that corrupts a secret is worse than the rotation it serves, because the old
// ciphertext is gone with the row it was replaced in.
//
// Retired rows are resealed too. Retiring sets disabled_at and leaves the
// ciphertext, and set-secret revives it in place, so a retired row sealed under
// a key that has been removed cannot be revived (cleat#1989).
//
// disabled_at and updated_at are left alone: a reseal changes which key protects
// a value, not the value, and "last changed" should keep meaning the latter.
//
// dryRun reads and verifies everything and writes nothing.
func (s *SecretStore) ResealSecrets(ctx context.Context, dryRun bool) (SecretReseal, error) {
	var res SecretReseal
	if s == nil || s.db == nil {
		return res, ErrNoSecretDB
	}
	if s.ring == nil {
		return res, ErrNoSecretMasterKey
	}
	cur := s.ring.current.Version

	err := s.forEachTenant(ctx, func(tctx context.Context, tid string) error {
		res.Tenants++
		type stored struct {
			name       string
			ciphertext string
			keyVersion int
		}
		var rows []stored
		if err := s.execTenantScoped(tctx, func(q querier) error {
			rs, err := q.QueryContext(tctx, listSecretsForResealStmt(s.dialect), tid)
			if err != nil {
				return fmt.Errorf("read secrets for tenant %s: %w", tid, err)
			}
			defer func() { _ = rs.Close() }()
			for rs.Next() {
				var r stored
				if err := rs.Scan(&r.name, &r.ciphertext, &r.keyVersion); err != nil {
					return fmt.Errorf("scan secret for tenant %s: %w", tid, err)
				}
				rows = append(rows, r)
			}
			return rs.Err()
		}); err != nil {
			return err
		}

		for _, r := range rows {
			res.Rows++
			if r.keyVersion == cur {
				res.Current++
				continue
			}
			plaintext, err := s.open(tid, r.ciphertext, r.keyVersion)
			if err != nil {
				res.Unreadable = append(res.Unreadable, UnreadableSecret{
					TenantID: tid, Name: r.name, KeyVersion: r.keyVersion, Reason: err.Error()})
				continue
			}
			next, err := s.seal(tid, plaintext)
			if err != nil {
				return fmt.Errorf("re-seal %q for tenant %s: %w", r.name, tid, err)
			}
			back, err := s.open(tid, next, cur)
			if err != nil {
				return fmt.Errorf("re-sealed %q for tenant %s does not open: %w", r.name, tid, err)
			}
			if back != plaintext {
				return fmt.Errorf("re-sealed %q for tenant %s does not round-trip; nothing further was written", r.name, tid)
			}
			if dryRun {
				res.Resealed++
				continue
			}
			if s.beforeResealWrite != nil {
				s.beforeResealWrite(tid, r.name)
			}
			var affected int64
			if err := s.gatedWrite(tctx, cur, func(q querier) error {
				out, err := q.ExecContext(tctx, resealSecretStmt(s.dialect),
					next, cur, tid, r.name, r.keyVersion, r.ciphertext)
				if err != nil {
					return err
				}
				affected, err = out.RowsAffected()
				return err
			}); err != nil {
				return fmt.Errorf("write re-sealed %q for tenant %s: %w", r.name, tid, err)
			}
			if affected == 0 {
				res.Changed++
			} else {
				res.Resealed++
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	sort.Slice(res.Unreadable, func(i, j int) bool {
		a, b := res.Unreadable[i], res.Unreadable[j]
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		return a.Name < b.Name
	})
	return res, nil
}

// listSecretsForResealStmt reads EVERY row of a tenant -- retired included, no
// disabled_at filter -- because a retired secret is still ciphertext sealed
// under a key that may be about to be removed.
func listSecretsForResealStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `SELECT name, ciphertext, key_version FROM tenant_secrets WHERE tenant_id = ? ORDER BY name`
	case "mssql":
		return `SELECT name, ciphertext, key_version FROM tenant_secrets WHERE tenant_id = @p1 ORDER BY name`
	default:
		return `SELECT name, ciphertext, key_version FROM tenant_secrets WHERE tenant_id = $1 ORDER BY name`
	}
}

// resealSecretStmt is the conditional write. The last two predicates are the
// compare-and-swap; see ResealSecrets for what removing them costs.
//
// The ciphertext comparison is safe against a coincidental match: every seal
// draws a fresh random 12-byte nonce, so two writes of the same plaintext do
// not produce the same stored value.
//
// MySQL reports rows CHANGED and not rows MATCHED (CLAUDE.md), so "0 affected"
// would not mean "no such row" in general. Here it does, because the SET always
// changes the ciphertext to a value that differs from the one matched.
func resealSecretStmt(dialect string) string {
	switch dialect {
	case "mysql":
		return `UPDATE tenant_secrets SET ciphertext = ?, key_version = ? WHERE tenant_id = ? AND name = ? AND key_version = ? AND ciphertext = ?`
	case "mssql":
		return `UPDATE tenant_secrets SET ciphertext = @p1, key_version = @p2 WHERE tenant_id = @p3 AND name = @p4 AND key_version = @p5 AND ciphertext = @p6`
	default:
		return `UPDATE tenant_secrets SET ciphertext = $1, key_version = $2 WHERE tenant_id = $3 AND name = $4 AND key_version = $5 AND ciphertext = $6`
	}
}
