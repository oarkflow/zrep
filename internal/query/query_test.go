package query

import "testing"

func TestParseBooleanFieldExpression(t *testing.T) {
	pred, err := Parse(`user_id = 42 AND (activity = "login" OR activity = "checkout") AND NOT status = 500`)
	if err != nil {
		t.Fatal(err)
	}
	if !pred.Match(Record{"user_id": "42", "activity": "checkout", "status": "200"}) {
		t.Fatal("expected record to match")
	}
	if pred.Match(Record{"user_id": "42", "activity": "checkout", "status": "500"}) {
		t.Fatal("expected NOT status predicate to reject")
	}
}

func TestParseContainsAndNumericComparison(t *testing.T) {
	pred, err := Parse(`email CONTAINS "example.com" AND status >= 400`)
	if err != nil {
		t.Fatal(err)
	}
	if !pred.Match(Record{"email": "ada@example.com", "status": "500"}) {
		t.Fatal("expected record to match")
	}
	if pred.Match(Record{"email": "ada@example.com", "status": "200"}) {
		t.Fatal("expected numeric comparison to reject")
	}
}
