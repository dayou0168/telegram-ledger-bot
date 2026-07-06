package bot

import "testing"

func TestCalculateReferenceDivision(t *testing.T) {
	result, err := calculateExpression("1000/6.8")
	if err != nil {
		t.Fatal(err)
	}
	if got := formatCalculationResult(result); got != "147.05882352941" {
		t.Fatalf("result = %s", got)
	}
}

func TestCalculateReferenceSubtractTwoDivisions(t *testing.T) {
	result, err := calculateExpression("1000/7.5-1000/6.8")
	if err != nil {
		t.Fatal(err)
	}
	if got := formatCalculationResult(result); got != "-13.725490196078" {
		t.Fatalf("result = %s", got)
	}
}

func TestCalculatePrecedenceAndParentheses(t *testing.T) {
	result, err := calculateExpression("(2+3)*4")
	if err != nil {
		t.Fatal(err)
	}
	if got := formatCalculationResult(result); got != "20" {
		t.Fatalf("result = %s", got)
	}
}

func TestArithmeticExpressionDetection(t *testing.T) {
	if !isArithmeticExpression("1000/6.8") {
		t.Fatal("expected arithmetic expression")
	}
	if !isArithmeticExpression("1000/7.5-1000/6.8") {
		t.Fatal("expected arithmetic expression")
	}
	if isArithmeticExpression("显示账单") {
		t.Fatal("text command should not be arithmetic expression")
	}
	if isArithmeticExpression("1000") {
		t.Fatal("plain number should not be arithmetic expression")
	}
}
