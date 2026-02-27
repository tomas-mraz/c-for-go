package translator

import (
	"strconv"

	"modernc.org/cc/v4"
)

// cBuiltinToGoType maps common C integer typedef names to their Go equivalents.
// Returns "" if the name is not a known builtin type.
func cBuiltinToGoType(cName string) string {
	switch cName {
	case "uint8_t":
		return "uint8"
	case "uint16_t":
		return "uint16"
	case "uint32_t":
		return "uint32"
	case "uint64_t":
		return "uint64"
	case "int8_t":
		return "int8"
	case "int16_t":
		return "int16"
	case "int32_t":
		return "int32"
	case "int64_t":
		return "int64"
	case "size_t", "uintptr_t":
		return "uintptr"
	case "intptr_t", "ptrdiff_t":
		return "uintptr"
	}
	return ""
}

// tryEvalWithMacroExpansion tries to evaluate a macro whose replacement list
// contains a function-like macro call. It expands the call by substituting
// argument values into the macro body, then evaluates the resulting C constant
// integer expression, resolving any remaining identifier references via the
// defines map.
//
// Example: VK_API_VERSION_1_1 = VK_MAKE_API_VERSION(0, 1, 1, 0)
// expands VK_MAKE_API_VERSION's body with variant=0, major=1, minor=1, patch=0
// and evaluates ((0<<29)|(1<<22)|(1<<12)|0) = 4198400.
func tryEvalWithMacroExpansion(tokens []cc.Token, defines map[string]*cc.Macro) (Value, bool) {
	expanded, ok := expandFnMacroCallInTokens(tokens, defines)
	if !ok {
		return nil, false
	}
	n, ok := evalCTokensAsIntWithDefines(expanded, defines)
	if !ok {
		return nil, false
	}
	return Value(cc.UInt64Value(n)), true
}

// expandFnMacroCallInTokens scans tokens for a function-like macro call
// (IDENTIFIER '(' args ')'), expands it by substituting parameters with
// argument tokens, and returns the resulting token list.
func expandFnMacroCallInTokens(tokens []cc.Token, defines map[string]*cc.Macro) ([]cc.Token, bool) {
	for i, tok := range tokens {
		if tok.Ch != rune(cc.IDENTIFIER) {
			continue
		}
		fnMacro, ok := defines[tok.SrcStr()]
		if !ok || !fnMacro.IsFnLike {
			continue
		}
		expanded, newEnd, ok := expandFnCall(fnMacro, tokens, i+1)
		if !ok {
			continue
		}
		result := make([]cc.Token, 0, i+len(expanded)+(len(tokens)-newEnd))
		result = append(result, tokens[:i]...)
		result = append(result, expanded...)
		result = append(result, tokens[newEnd:]...)
		// Recursively expand nested macro calls.
		if result2, ok2 := expandFnMacroCallInTokens(result, defines); ok2 {
			return result2, true
		}
		return result, true
	}
	return nil, false
}

// expandFnCall expands a function-like macro call. argStart must point to the
// '(' token. Returns the expanded token list and the position after ')'.
func expandFnCall(fnMacro *cc.Macro, tokens []cc.Token, argStart int) ([]cc.Token, int, bool) {
	if argStart >= len(tokens) || tokens[argStart].SrcStr() != "(" {
		return nil, argStart, false
	}
	params := fnMacro.Params // []cc.Token – parameter name tokens

	// Collect comma-separated argument token groups.
	var args [][]cc.Token
	var currentArg []cc.Token
	depth := 0
	i := argStart + 1 // skip '('
	for i < len(tokens) {
		src := tokens[i].SrcStr()
		switch {
		case src == "(":
			depth++
			currentArg = append(currentArg, tokens[i])
		case src == ")":
			if depth == 0 {
				args = append(args, currentArg)
				i++ // skip ')'
				goto done
			}
			depth--
			currentArg = append(currentArg, tokens[i])
		case src == "," && depth == 0:
			args = append(args, currentArg)
			currentArg = nil
		default:
			currentArg = append(currentArg, tokens[i])
		}
		i++
	}
done:
	if len(args) != len(params) {
		return nil, argStart, false
	}

	// Map each parameter name to its argument token list.
	paramMap := make(map[string][]cc.Token, len(params))
	for j, param := range params {
		paramMap[param.SrcStr()] = args[j]
	}

	// Substitute parameters in the replacement list.
	replList := fnMacro.ReplacementList()
	expanded := make([]cc.Token, 0, len(replList)*2)
	for _, tok := range replList {
		if tok.Ch == rune(cc.IDENTIFIER) {
			if argToks, ok := paramMap[tok.SrcStr()]; ok {
				expanded = append(expanded, argToks...)
				continue
			}
		}
		expanded = append(expanded, tok)
	}
	return expanded, i, true
}

// evalCTokensAsIntWithDefines evaluates a C constant expression (as a token
// list) to a uint64. When an identifier is encountered, it is looked up in the
// defines map and its replacement list is recursively evaluated.
// Handles integer literals, parentheses, type casts (skipped), and
// the operators: |, ^, &, <<, >>, +, -, *, /, %, ~.
func evalCTokensAsIntWithDefines(tokens []cc.Token, defines map[string]*cc.Macro) (uint64, bool) {
	strs := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if s := tok.SrcStr(); s != "" {
			strs = append(strs, s)
		}
	}
	if len(strs) == 0 {
		return 0, false
	}
	p := &cIntParser{toks: strs, defines: defines}
	val, ok := p.parseOr()
	if !ok || p.pos < len(p.toks) {
		return 0, false
	}
	return val, true
}

type cIntParser struct {
	toks    []string
	pos     int
	defines map[string]*cc.Macro // for resolving identifier references
}

func (p *cIntParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *cIntParser) consume() string {
	s := p.peek()
	if s != "" {
		p.pos++
	}
	return s
}

func (p *cIntParser) parseOr() (uint64, bool) {
	left, ok := p.parseXor()
	if !ok {
		return 0, false
	}
	for p.peek() == "|" {
		p.consume()
		right, ok := p.parseXor()
		if !ok {
			return 0, false
		}
		left |= right
	}
	return left, true
}

func (p *cIntParser) parseXor() (uint64, bool) {
	left, ok := p.parseAnd()
	if !ok {
		return 0, false
	}
	for p.peek() == "^" {
		p.consume()
		right, ok := p.parseAnd()
		if !ok {
			return 0, false
		}
		left ^= right
	}
	return left, true
}

func (p *cIntParser) parseAnd() (uint64, bool) {
	left, ok := p.parseShift()
	if !ok {
		return 0, false
	}
	for p.peek() == "&" {
		p.consume()
		right, ok := p.parseShift()
		if !ok {
			return 0, false
		}
		left &= right
	}
	return left, true
}

func (p *cIntParser) parseShift() (uint64, bool) {
	left, ok := p.parseAdditive()
	if !ok {
		return 0, false
	}
	for p.peek() == "<<" || p.peek() == ">>" {
		op := p.consume()
		right, ok := p.parseAdditive()
		if !ok {
			return 0, false
		}
		if op == "<<" {
			left <<= right
		} else {
			left >>= right
		}
	}
	return left, true
}

func (p *cIntParser) parseAdditive() (uint64, bool) {
	left, ok := p.parseMultiplicative()
	if !ok {
		return 0, false
	}
	for p.peek() == "+" || p.peek() == "-" {
		op := p.consume()
		right, ok := p.parseMultiplicative()
		if !ok {
			return 0, false
		}
		if op == "+" {
			left += right
		} else {
			left -= right
		}
	}
	return left, true
}

func (p *cIntParser) parseMultiplicative() (uint64, bool) {
	left, ok := p.parseUnary()
	if !ok {
		return 0, false
	}
	for p.peek() == "*" || p.peek() == "/" || p.peek() == "%" {
		op := p.consume()
		right, ok := p.parseUnary()
		if !ok {
			return 0, false
		}
		switch op {
		case "*":
			left *= right
		case "/":
			if right == 0 {
				return 0, false
			}
			left /= right
		case "%":
			if right == 0 {
				return 0, false
			}
			left %= right
		}
	}
	return left, true
}

func (p *cIntParser) parseUnary() (uint64, bool) {
	switch p.peek() {
	case "-":
		p.consume()
		val, ok := p.parsePrimary()
		return ^val + 1, ok
	case "~":
		p.consume()
		val, ok := p.parsePrimary()
		return ^val, ok
	case "+":
		p.consume()
	}
	return p.parsePrimary()
}

// parsePrimary parses a numeric literal, identifier, or parenthesized expression.
// Type casts of the form (identifier) are detected and skipped.
func (p *cIntParser) parsePrimary() (uint64, bool) {
	s := p.peek()
	if s == "" {
		return 0, false
	}

	// Integer literal.
	if s[0] >= '0' && s[0] <= '9' {
		p.consume()
		raw := stripNumericSuffixes(s)
		n, err := strconv.ParseUint(raw, 0, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}

	// Identifier: try to resolve via defines map (e.g. VK_HEADER_VERSION → 240).
	if isIdentRune(s) && p.defines != nil {
		if m, ok := p.defines[s]; ok && !m.IsFnLike {
			p.consume()
			replToks := m.ReplacementList()
			return evalCTokensAsIntWithDefines(replToks, p.defines)
		}
	}

	// Parenthesized expression or type cast.
	if s == "(" {
		p.consume()

		// Detect a C type cast: '(' type-name ')' followed by another primary.
		// A type-name consists of identifiers (e.g. "uint32_t"). We distinguish a
		// type cast from a grouped constant expression by checking that the identifier
		// is NOT a known macro constant (which would be in p.defines).
		if isIdentRune(p.peek()) {
			firstIdent := p.peek()
			isKnownConst := p.defines != nil && func() bool {
				m, ok := p.defines[firstIdent]
				return ok && !m.IsFnLike
			}()
			if !isKnownConst {
				saved := p.pos
				// Scan identifiers and '*' until we see ')'.
				for isIdentRune(p.peek()) || p.peek() == "*" {
					p.consume()
				}
				if p.peek() == ")" {
					p.consume() // consume ')'
					// What follows must be another primary (the cast operand).
					return p.parsePrimary()
				}
				// Not a type cast; restore and parse as grouped expression.
				p.pos = saved
			}
		}

		val, ok := p.parseOr()
		if !ok {
			return 0, false
		}
		if p.consume() != ")" {
			return 0, false
		}
		return val, true
	}

	return 0, false
}

// isIdentRune reports whether s looks like a C identifier.
func isIdentRune(s string) bool {
	if len(s) == 0 {
		return false
	}
	c := s[0]
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// stripNumericSuffixes removes C integer suffixes (u/U/l/L) from a numeric literal.
func stripNumericSuffixes(s string) string {
	for len(s) > 0 {
		last := s[len(s)-1]
		if last == 'u' || last == 'U' || last == 'l' || last == 'L' {
			s = s[:len(s)-1]
		} else {
			break
		}
	}
	return s
}
