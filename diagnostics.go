package main

import (
	"github.com/lonelycode/go-uuid/uuid"
	"io/ioutil"
	"time"
)

type UsageDiagnostic struct {
	APICount       int
	TokenCount     int
	InstallationID string
	Timestamp      time.Time
}

const diagnosticPath string = "/tmp/.tyk_diagnostics"

type Diagnostics struct {
	InstallationID string
	DiagnosticData UsageDiagnostic
}

func (d Diagnostics) New() *Diagnostics {
	thisD := &Diagnostics{}
	ok, id := thisD.InitDiagnostic()
	if ok {
		thisD.InstallationID = id
		thisD.DiagnosticData = UsageDiagnostic{
			InstallationID: id,
		}
	}
	return thisD
}

func (d *Diagnostics) SetAPICount(count int) {
	d.DiagnosticData.APICount = count
}

func (d *Diagnostics) IncrementTokenCount() {
	log.Debug("Incrementing token count")
	d.DiagnosticData.TokenCount += 1
}

func (d *Diagnostics) InitTokenCount(count int) {
	log.Debug("Init token count")
	d.DiagnosticData.TokenCount = count
}

func (d *Diagnostics) DecrementTokenCount() {
	if d.DiagnosticData.TokenCount > 0 {
		d.DiagnosticData.TokenCount -= 1
	}
}

func (d Diagnostics) WriteHook() (string, error) {
	id := uuid.NewUUID().String()
	err := ioutil.WriteFile(diagnosticPath, []byte(id), 0644)
	return id, err
}

func (d Diagnostics) InitDiagnostic() (bool, string) {
	var hook_id string
	var gaveUp bool
	data, err := ioutil.ReadFile(diagnosticPath)

	if err != nil {
		var getIdErr error
		hook_id, getIdErr = d.WriteHook()
		if getIdErr != nil {
			gaveUp = true
		}
	}

	if gaveUp {
		return false, ""
	}

	if hook_id == "" {
		hook_id = string(data)
	}

	return true, hook_id
}

func (d *Diagnostics) ReportDiagnostic() {
	d.DiagnosticData.Timestamp = time.Now()
	log.Info("Diagnostic data")
	log.Info(d.DiagnosticData)
}

func (d *Diagnostics) StartWaitLoop(sleepTime int) {
	for {
		d.ReportDiagnostic()
		time.Sleep(5 * time.Second)
	}
}
