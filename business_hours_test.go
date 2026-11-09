package main

import "testing"

func TestFindVacation(t *testing.T) {
	periods := []VacationPeriod{
		{From: "2026-12-23", To: "2027-01-03", Announcement: "xmas.wav"},
		{From: "2027-08-01", To: "2027-08-01"},
	}
	cases := map[string]string{ // day -> From of the matching period, "" if none
		"2026-12-22": "",
		"2026-12-23": "2026-12-23", // first day
		"2026-12-31": "2026-12-23",
		"2027-01-01": "2026-12-23", // across the new year
		"2027-01-03": "2026-12-23", // last day
		"2027-01-04": "",
		"2027-08-01": "2027-08-01", // one-day period
		"2027-08-02": "",
	}
	for day, want := range cases {
		got := ""
		if p := findVacation(periods, day); p != nil {
			got = p.From
		}
		if got != want {
			t.Errorf("findVacation(%s) = %q, want %q", day, got, want)
		}
	}
	if p := findVacation(periods, "2026-12-25"); p == nil || p.Announcement != "xmas.wav" {
		t.Errorf("per-period announcement lost: %+v", p)
	}
}
