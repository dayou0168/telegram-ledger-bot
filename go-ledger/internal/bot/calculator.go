package bot

import (
	"errors"
	"math/big"
	"strings"
	"unicode"
)

var errCalculator = errors.New("invalid arithmetic expression")

type calcStream struct {
	tokens []string
	index  int
}

func (s *calcStream) peek() string {
	if s.index >= len(s.tokens) {
		return ""
	}
	return s.tokens[s.index]
}

func (s *calcStream) pop() (string, error) {
	token := s.peek()
	if token == "" {
		return "", errCalculator
	}
	s.index++
	return token, nil
}

func isArithmeticExpression(text string) bool {
	stripped := strings.TrimSpace(text)
	if stripped == "" {
		return false
	}
	hasDigit := false
	hasOperator := false
	for _, r := range stripped {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case strings.ContainsRune("+-*/", r):
			hasOperator = true
		case r == '.' || r == '(' || r == ')' || unicode.IsSpace(r):
		default:
			return false
		}
	}
	return hasDigit && hasOperator
}

func calculateExpression(text string) (*big.Rat, error) {
	tokens, err := tokenizeExpression(text)
	if err != nil || len(tokens) == 0 {
		return nil, errCalculator
	}
	stream := &calcStream{tokens: tokens}
	value, err := parseCalcExpr(stream)
	if err != nil {
		return nil, err
	}
	if stream.peek() != "" {
		return nil, errCalculator
	}
	return value, nil
}

func tokenizeExpression(text string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(text); {
		c := text[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			i++
			continue
		}
		if strings.ContainsRune("+-*/()", rune(c)) {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if (c >= '0' && c <= '9') || c == '.' {
			start := i
			dots := 0
			for i < len(text) && ((text[i] >= '0' && text[i] <= '9') || text[i] == '.') {
				if text[i] == '.' {
					dots++
					if dots > 1 {
						return nil, errCalculator
					}
				}
				i++
			}
			token := text[start:i]
			if token == "." {
				return nil, errCalculator
			}
			tokens = append(tokens, token)
			continue
		}
		return nil, errCalculator
	}
	return tokens, nil
}

func parseCalcExpr(stream *calcStream) (*big.Rat, error) {
	value, err := parseCalcTerm(stream)
	if err != nil {
		return nil, err
	}
	for stream.peek() == "+" || stream.peek() == "-" {
		op, _ := stream.pop()
		right, err := parseCalcTerm(stream)
		if err != nil {
			return nil, err
		}
		if op == "+" {
			value.Add(value, right)
		} else {
			value.Sub(value, right)
		}
	}
	return value, nil
}

func parseCalcTerm(stream *calcStream) (*big.Rat, error) {
	value, err := parseCalcFactor(stream)
	if err != nil {
		return nil, err
	}
	for stream.peek() == "*" || stream.peek() == "/" {
		op, _ := stream.pop()
		right, err := parseCalcFactor(stream)
		if err != nil {
			return nil, err
		}
		if op == "*" {
			value.Mul(value, right)
			continue
		}
		if right.Sign() == 0 {
			return nil, errCalculator
		}
		value.Quo(value, right)
	}
	return value, nil
}

func parseCalcFactor(stream *calcStream) (*big.Rat, error) {
	token, err := stream.pop()
	if err != nil {
		return nil, err
	}
	switch token {
	case "+":
		return parseCalcFactor(stream)
	case "-":
		value, err := parseCalcFactor(stream)
		if err != nil {
			return nil, err
		}
		return value.Neg(value), nil
	case "(":
		value, err := parseCalcExpr(stream)
		if err != nil {
			return nil, err
		}
		next, err := stream.pop()
		if err != nil || next != ")" {
			return nil, errCalculator
		}
		return value, nil
	default:
		value := new(big.Rat)
		if _, ok := value.SetString(token); !ok {
			return nil, errCalculator
		}
		return value, nil
	}
}

func formatCalculationResult(value *big.Rat) string {
	if value == nil {
		return "0"
	}
	f := new(big.Float).SetPrec(256).SetRat(value)
	text := f.Text('g', 14)
	if text == "-0" || text == "" {
		return "0"
	}
	return text
}
