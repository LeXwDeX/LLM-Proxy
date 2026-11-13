package proxy

import (
	"encoding/json"
	"testing"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/LeXwDeX/LLM-Proxy/internal/nosql"
	bolt "go.etcd.io/bbolt"
)

// grantWholeTargets puts an unlimited model group bound to each whole target
// into client's plan: quota is only granted through plan groups, so fixtures
// that only test billing or accounting must grant their routes explicitly.
func grantWholeTargets(t *testing.T, db *bolt.DB, client string, targets ...string) {
	t.Helper()
	var bindings []config.QuotaGroupBinding
	for _, target := range targets {
		bindings = append(bindings, config.QuotaGroupBinding{Target: target})
	}
	put := func(bucket, key string, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(bucket)).Put([]byte(key), raw) }); err != nil {
			t.Fatal(err)
		}
	}
	put(nosql.BucketModelGroups, "granted", config.ModelGroup{Name: "granted", CostCoefficient: 1, Bindings: bindings})
	put(nosql.BucketClientQuotaGroups, client, struct {
		Name   string              `json:"name"`
		Groups []config.QuotaGroup `json:"groups"`
	}{client, []config.QuotaGroup{{Name: "granted"}}})
}
