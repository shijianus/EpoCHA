package main

import (
 "os"
 "path/filepath"
 "testing"
)

func TestInvalidMeasurementArgumentsDoNotWrite(t *testing.T) {
 for _, args:=range [][]string{{"--generators=,"},{"--generators="},{"--difficulty=0"},{"--generators=tile-rotation-v1,tile-rotation-v1"},{"--challenges=0"}} {
  t.Run(args[0],func(t *testing.T){
   out:=filepath.Join(t.TempDir(),"result")
   call:=append([]string{"--challenges=1","--out-dir",out},args...)
   if code:=run(call);code!=1{t.Fatalf("invalid args code=%d want 1",code)}
   if _,err:=os.Stat(out);!os.IsNotExist(err){t.Fatalf("invalid args created output: %v",err)}
  })
 }
}

func TestMeasurementRefusesExistingOutputDirectory(t *testing.T) {
 out:=t.TempDir()
 sentinel:=filepath.Join(out,"m4-summary.json")
 if err:=os.WriteFile(sentinel,[]byte("historical-evidence"),0600);err!=nil{t.Fatal(err)}
 if code:=run([]string{"--challenges=1","--generators=tile-rotation-v1","--out-dir",out});code!=1{t.Fatalf("existing output code=%d",code)}
 body,err:=os.ReadFile(sentinel);if err!=nil||string(body)!="historical-evidence"{t.Fatalf("historical evidence overwritten: %v",err)}
}
