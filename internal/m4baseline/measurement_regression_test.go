package m4baseline

import (
 "bytes"
 "encoding/csv"
 "strconv"
 "testing"
)

func TestTimingSparseCPUUsesWallProxy(t *testing.T) {
 rows:=make([]ChallengeRecord,100)
 for i:=range rows {rows[i]=ChallengeRecord{Solved:true,SolverWallMs:2};if i==99{rows[i].SolverCPUMs=16}}
 got:=summarizeGenerator(GeneratorResult{Solved:100,KeepExperimental:true},rows)
 if got.P95TimingBasis=="cpu" {t.Fatal("sparse jump counter cannot substantiate per-question CPU p95")}
 if got.P95CPUMs==0 && got.P95TimingBasis!="wall-proxy" {t.Fatalf("zero CPU mislabeled: %+v",got)}
}

func TestEmptySummaryHasUninformativeInterval(t *testing.T) {
 got:=summarizeGenerator(GeneratorResult{},nil)
 if got.CI95Low!=0||got.CI95High!=1||got.Verdict!="BLOCKED" {t.Fatalf("empty evidence: %+v",got)}
}

func TestCSVPreservesTimingPrecision(t *testing.T) {
 want:=0.123456789123
 var out bytes.Buffer
 if err:=WriteCSV(&out,[]ChallengeRecord{{Generator:"test",SolverWallMs:want}});err!=nil{t.Fatal(err)}
 records,err:=csv.NewReader(&out).ReadAll();if err!=nil{t.Fatal(err)}
 col:=-1;for i,h:=range records[0]{if h=="solver_wall_ms"{col=i}}
 if col<0{t.Fatal("timing column missing")}
 got,err:=strconv.ParseFloat(records[1][col],64);if err!=nil||got!=want{t.Fatalf("CSV lost evidence precision got=%v want=%v err=%v",got,want,err)}
}
