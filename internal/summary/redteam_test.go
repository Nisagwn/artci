package summary

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/nisaguven/artci/internal/normalize"
)

type redteamFile struct {
	Events    map[string]normalize.Event `json:"events"`
	Scenarios []struct {
		Name     string `json:"name"`
		Category string `json:"category"`
		Event    string `json:"event"`
		Output   string `json:"output"`
		Want     string `json:"want"`
		Code     string `json:"code"`
	} `json:"scenarios"`
}

// TestRedTeam, kayıtlı model yanıtlarını doğrulayıcıdan geçirir (Plan §11).
// CI'da her commit'te çalışır: go test ./internal/summary -run RedTeam
func TestRedTeam(t *testing.T) {
	b, err := os.ReadFile("../../testdata/redteam/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var rf redteamFile
	if err := json.Unmarshal(b, &rf); err != nil {
		t.Fatal(err)
	}
	rejects := 0
	for _, sc := range rf.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			ev, ok := rf.Events[sc.Event]
			if !ok {
				t.Fatalf("bilinmeyen olay %q", sc.Event)
			}
			f := BuildFacts(ev)
			text, err := ParseModelOutput(sc.Output)
			if err == nil {
				err = Validate(text, f)
			}
			switch sc.Want {
			case "accept":
				if err != nil {
					t.Errorf("kabul edilmeliydi: %v\nçıktı: %s", err, sc.Output)
				}
			case "reject":
				if err == nil {
					t.Errorf("reddedilmeliydi\nçıktı: %s", sc.Output)
					return
				}
				if r, ok := IsRejection(err); !ok || (sc.Code != "" && r.Code != sc.Code) {
					t.Errorf("ret kodu %v, beklenen %q", err, sc.Code)
				}
			default:
				t.Fatalf("want=%q", sc.Want)
			}
		})
		if sc.Want == "reject" {
			rejects++
		}
	}
	if rejects < 30 {
		t.Errorf("Plan §11: en az 30 ret senaryosu olmalı, %d var", rejects)
	}
}
