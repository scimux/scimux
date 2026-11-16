package team

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The overlay lets this test commit build before the new people API exists.
func TestPeopleContract(t *testing.T) {
	dir := t.TempDir()
	generated := filepath.Join(dir, "people_generated_test.go")
	if err := os.WriteFile(generated, []byte(peopleContractTest), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(cwd, "people_generated_test.go"): generated}})
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(config, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "test", "-overlay="+config, "-run=^TestPeopleContractGenerated$", "-count=1", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("people contract: %v\n%s", err, out)
	}
}

const peopleContractTest = `package team
import ("bytes"; "errors"; "fmt"; "os"; "path/filepath"; "strings"; "testing")
func TestPeopleContractGenerated(t *testing.T) {
 const server = "11111111111111111111111111111111"
 const seat = "22222222222222222222222222222222"
 t.Run("missing and sorted", func(t *testing.T) {
  dir := t.TempDir(); got, err := LoadPeople(dir); if err != nil || len(got) != 0 { t.Fatalf("missing: %v %v", got, err) }
  rows := []Person{{Server:server,Seat:seat,Name:"Ada"},{Server:server,Seat:"00000000000000000000000000000000",Name:"Bea"}}
  if err := SavePeople(dir, rows); err != nil { t.Fatal(err) }; got, err = LoadPeople(dir); if err != nil || len(got) != 2 || got[0].Name != "Bea" { t.Fatalf("sorted: %v %v", got, err) }
  info, err := os.Stat(filepath.Join(dir, PeopleFile)); if err != nil || info.Mode().Perm() != 0600 { t.Fatal("permissions") }
 })
 for name, raw := range map[string]string{
  "empty":"", "json":"{", "version":"{\"v\":2,\"people\":[]}", "missing version":"{\"people\":[]}",
  "missing people":"{\"v\":1}", "null people":"{\"v\":1,\"people\":null}", "unknown":"{\"v\":1,\"people\":[],\"x\":1}", "extra":"{\"v\":1,\"people\":[]} {}",
  "server":"{\"v\":1,\"people\":[{\"server\":\"bad\",\"seat\":\""+seat+"\",\"name\":\"Ada\"}]}",
  "seat":"{\"v\":1,\"people\":[{\"server\":\""+server+"\",\"seat\":\"bad\",\"name\":\"Ada\"}]}",
  "name":"{\"v\":1,\"people\":[{\"server\":\""+server+"\",\"seat\":\""+seat+"\",\"name\":\" Ada \"}]}",
  "duplicate":"{\"v\":1,\"people\":[{\"server\":\""+server+"\",\"seat\":\""+seat+"\",\"name\":\"Ada\"},{\"server\":\""+server+"\",\"seat\":\""+seat+"\",\"name\":\"Ada\"}]}",
  "large":strings.Repeat(" ", (1<<20)+1), "utf8":string([]byte{0xff}),
 } { t.Run(name, func(t *testing.T) {
   dir := t.TempDir(); path := filepath.Join(dir, PeopleFile); if err := os.WriteFile(path, []byte(raw), 0600); err != nil { t.Fatal(err) }
   if _, err := LoadPeople(dir); !errors.Is(err, ErrPeopleDamaged) { t.Fatalf("load: %v", err) }
   if err := SavePeople(dir, nil); err == nil { t.Fatal("overwrote damaged file") }; after, err := os.ReadFile(path); if err != nil || !bytes.Equal(after, []byte(raw)) { t.Fatal("damage overwritten") }
  }) }
 t.Run("limit", func(t *testing.T) { dir:=t.TempDir(); rows:=make([]Person,4097); for i:=range rows { rows[i]=Person{Server:server,Seat:fmt.Sprintf("%032x",i),Name:"Ada"} }; if err:=SavePeople(dir,rows); err==nil { t.Fatal("accepted too many") } })
}
`
