package main

import (
	"reflect"
	"testing"
)

func TestDecodeColorCodesInput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "empty", input: "", want: []string{}},
		{name: "comma separated", input: "ABC, DEF, GHI", want: []string{"ABC", "DEF", "GHI"}},
		{name: "trailing separator", input: "ABC, DEF,", want: []string{"ABC", "DEF"}},
		{name: "too many", input: "1,2,3,4,5,6,7,8,9", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeColorCodesInput(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("decodeColorCodesInput() error = %v, wantErr %v", err, test.wantErr)
			}
			if !test.wantErr && !reflect.DeepEqual(got, test.want) {
				t.Fatalf("decodeColorCodesInput() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestSetStringArrayValue(t *testing.T) {
	values := []string{"one", "two"}
	got, err := setStringArrayValue(values, 1, "updated")
	if err != nil {
		t.Fatalf("setStringArrayValue() error = %v", err)
	}
	want := []string{"one", "updated", "", "", "", "", "", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("setStringArrayValue() = %#v, want %#v", got, want)
	}

	if _, err := setStringArrayValue(values, colorwayCount, "invalid"); err == nil {
		t.Fatal("setStringArrayValue() expected an error for an invalid index")
	}
}

func TestValidateComponents(t *testing.T) {
	tests := []struct {
		name       string
		components []string
		wantErr    bool
	}{
		{name: "empty", components: []string{}},
		{name: "supported categories", components: []string{"upper", "lining"}},
		{name: "null list", components: nil, wantErr: true},
		{name: "unknown category", components: []string{"unknown"}, wantErr: true},
		{name: "duplicate category", components: []string{"upper", "upper"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateComponents(test.components); (err != nil) != test.wantErr {
				t.Fatalf("validateComponents() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
