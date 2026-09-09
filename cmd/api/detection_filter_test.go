package main

import "testing"

func TestDetectionFilterValidateClampsMinConf(t *testing.T) {
	f := DetectionFilter{MinConf: -0.5}
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if f.MinConf != 0 {
		t.Errorf("MinConf = %v, want clamped to 0", f.MinConf)
	}

	f = DetectionFilter{MinConf: 1.5}
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if f.MinConf != 1 {
		t.Errorf("MinConf = %v, want clamped to 1", f.MinConf)
	}
}

func TestDetectionFilterValidateRejectsUnknownClass(t *testing.T) {
	f := DetectionFilter{Classes: []string{"car", "spaceship"}}
	if err := f.Validate(); err == nil {
		t.Fatal("Validate() = nil, want error for unknown class")
	}
}

func TestDetectionFilterValidateAcceptsKnownClasses(t *testing.T) {
	f := DetectionFilter{MinConf: 0.3, Classes: []string{"car", "person"}}
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDetectionFilterAllowsClass(t *testing.T) {
	empty := DetectionFilter{}
	if !empty.allowsClass("car") {
		t.Error("empty Classes should allow every class")
	}

	restricted := DetectionFilter{Classes: []string{"car", "truck"}}
	if !restricted.allowsClass("car") {
		t.Error("restricted filter should allow a listed class")
	}
	if restricted.allowsClass("person") {
		t.Error("restricted filter should reject an unlisted class")
	}
}

func TestDefaultDetectionFilter(t *testing.T) {
	f := DefaultDetectionFilter()
	if f.MinConf != 0.5 || !f.Objects || !f.Plates || !f.Vehicles || len(f.Classes) != 0 {
		t.Errorf("DefaultDetectionFilter = %+v, want min_conf=0.5, all kinds on, no classes", f)
	}
	if err := f.Validate(); err != nil {
		t.Errorf("default filter must itself validate: %v", err)
	}
}
