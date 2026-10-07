package engine

import (
	"context"
	"fmt"
)

// DeploymentSecretKeyCheck mirrors SecretKeyCheck for deployment secrets --
// see that type's doc comment for what each field means. There is no tenant
// dimension here (DeploymentSecretStore's own doc comment explains why), so
// Total is simply every row in deployment_secrets rather than a sum across
// tenants.
type DeploymentSecretKeyCheck struct {
	Total      int
	Unopenable map[int]int
	Configured []int
	OnPrevious map[int]int
}

// CheckKeyRingCandidate is SecretStore.CheckKeyRingCandidate's counterpart
// for deployment secrets -- cleat#2298's M4, extended to the half of "every
// stored secret" that CheckKeyRing never covered: nothing before this asked
// whether a candidate ring (or even the live one) opens every deployment
// secret, because deployment secrets have no boot-time equivalent of
// checkSecretsUsable (DeploymentSecretStore's own doc comment already flags
// this gap, for the write gate specifically; the read-side check had the
// same gap and nothing had named it until this).
//
// A nil candidate reports every row as unopenable, matching
// CheckKeyRingCandidate's convention for tenant secrets.
func (s *DeploymentSecretStore) CheckKeyRingCandidate(ctx context.Context, candidate *KeyRing) (DeploymentSecretKeyCheck, error) {
	chk := DeploymentSecretKeyCheck{Unopenable: map[int]int{}, OnPrevious: map[int]int{}, Configured: candidate.Versions()}
	if s == nil || s.db == nil {
		return chk, ErrNoDeploymentSecretDB
	}
	rows, err := s.db.QueryContext(ctx, deploymentSecretKeyVersionCountsStmt(s.dialect))
	if err != nil {
		return chk, fmt.Errorf("count deployment secrets by key_version: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var version, n int
		if err := rows.Scan(&version, &n); err != nil {
			return chk, fmt.Errorf("scan deployment secret key_version count: %w", err)
		}
		chk.Total += n
		switch {
		case candidate == nil:
			chk.Unopenable[version] = n
		case version == candidate.Current().Version:
		default:
			if _, ok := candidate.Key(version); ok {
				chk.OnPrevious[version] = n
			} else {
				chk.Unopenable[version] = n
			}
		}
	}
	if err := rows.Err(); err != nil {
		return chk, fmt.Errorf("count deployment secrets by key_version: %w", err)
	}
	return chk, nil
}

func deploymentSecretKeyVersionCountsStmt(dialect string) string {
	return `SELECT key_version, count(*) FROM deployment_secrets GROUP BY key_version`
}
