package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/scimux/scimux/internal/team"
)

func localStageRevision(stage team.PeopleStage) string {
	raw, _ := json.Marshal(stage)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
