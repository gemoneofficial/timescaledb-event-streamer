/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements. See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package tablefiltering

import (
	"fmt"
	"github.com/go-errors/errors"
	"github.com/noctarius/timescaledb-event-streamer/spi/systemcatalog"
	"regexp"
	"strings"
	"unicode"
)

type TableFilter struct {
	includes          []*filter
	excludes          []*filter
	filterCache       map[string]bool
	acceptedByDefault bool
}

func NewTableFilter(
	excludes, includes []string, acceptedByDefault bool,
) (*TableFilter, error) {

	excludeFilters := make([]*filter, 0)
	for _, exclude := range excludes {
		f, err := parseFilter(exclude)
		if err != nil {
			return nil, err
		}
		excludeFilters = append(excludeFilters, f)
	}

	includeFilters := make([]*filter, 0)
	for _, include := range includes {
		f, err := parseFilter(include)
		if err != nil {
			return nil, err
		}
		includeFilters = append(includeFilters, f)
	}

	return &TableFilter{
		includes:          includeFilters,
		excludes:          excludeFilters,
		filterCache:       make(map[string]bool, 0),
		acceptedByDefault: acceptedByDefault,
	}, nil
}

func (rf *TableFilter) Enabled(
	table systemcatalog.SystemEntity,
) bool {

	// already tested?
	canonicalName := table.CanonicalName()
	if v, present := rf.filterCache[canonicalName]; present {
		return v
	}

	// excluded has priority
	for _, exclude := range rf.excludes {
		if exclude.matches(table) {
			rf.filterCache[canonicalName] = false
			return false
		}
	}

	// is explicitly included?
	for _, include := range rf.includes {
		if include.matches(table) {
			rf.filterCache[canonicalName] = true
			return true
		}
	}

	// otherwise use acceptedByDefault
	rf.filterCache[canonicalName] = rf.acceptedByDefault
	return rf.acceptedByDefault
}

type filter struct {
	namespace      string
	table          string
	namespaceRegex *regexp.Regexp
	tableRegex     *regexp.Regexp
}

func parseFilter(
	filterTerm string,
) (*filter, error) {

	tokens := strings.Split(filterTerm, ".")
	if len(tokens) != 2 {
		return nil, fmt.Errorf("failed parsing filter term: %s", filterTerm)
	}

	namespace, namespaceIsRegex, err := parseToken(tokens[0])
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	table, tableIsRegex, err := parseToken(tokens[1])
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	f := &filter{}
	if namespaceIsRegex {
		f.namespaceRegex = regexp.MustCompile(fmt.Sprintf("^%s$", namespace))
	} else {
		f.namespace = namespace
	}

	if tableIsRegex {
		f.tableRegex = regexp.MustCompile(fmt.Sprintf("^%s$", table))
	} else {
		f.table = table
	}
	return f, nil
}

func (f *filter) matches(
	table systemcatalog.SystemEntity,
) bool {

	namespace := table.SchemaName()
	entity := table.TableName()

	// If the table is a hypertable, we want to check for a
	// potentially matching continuous aggregate
	if hypertable, ok := table.(*systemcatalog.Hypertable); ok {
		if hypertable.IsContinuousAggregate() {
			if n, found := hypertable.ViewSchema(); found {
				namespace = n
			} else {
				return false
			}
			if e, found := hypertable.ViewName(); found {
				entity = e
			} else {
				return false
			}
		}
	}

	if f.namespaceRegex != nil {
		if !f.namespaceRegex.Match([]byte(namespace)) {
			return false
		}
	} else {
		if f.namespace != namespace {
			return false
		}
	}

	if f.tableRegex != nil {
		if !f.tableRegex.Match([]byte(entity)) {
			return false
		}
	} else {
		if f.table != entity {
			return false
		}
	}
	return true
}

func parseToken(
	token string,
) (string, bool, error) {

	isQuoted := token[0] == '"' && token[len(token)-1] == '"'

	// When not quoted, all identifiers are folded to lowercase
	if !isQuoted {
		token = strings.ToLower(token)
	}

	// Check identifier length
	if len(token) > 63 {
		if !isQuoted || len(token) > 65 {
			return "", false, errors.Errorf("an pattern cannot be longer than 63 characters")
		}
	}

	firstIndex := 0
	if isQuoted {
		firstIndex++
	}
	lastIndex := len(token)
	if isQuoted {
		lastIndex--
	}

	// If unquoted the first character needs to be a letter, underscore, or a valid wildcard (*|?|+)
	if !isQuoted {
		if !unicode.IsLetter(rune(token[0])) &&
			token[0] != '_' &&
			token[0] != '*' &&
			token[0] != '?' &&
			token[0] != '+' {

			return "", false, errors.Errorf(
				"%s is an illegal first character of pattern '%s'", string(token[0]), token,
			)
		}
	}

	isRegex := false
	runedToken := []rune(token)
	builder := strings.Builder{}
	for i := firstIndex; i < lastIndex; i++ {
		char := runedToken[i]

		if char == '\\' && isQuoted {
			if i < len(runedToken)-1 {
				peekNextChar := runedToken[i+1]
				switch peekNextChar {
				case '*':
					fallthrough
				case '?':
					fallthrough
				case '+':
					builder.WriteString("\\")
					builder.WriteRune(peekNextChar)
					i++
				}
			}
		} else if char == '"' && isQuoted {
			if i < len(runedToken)-1 {
				peekNextChar := runedToken[i+1]
				builder.WriteString("\"\"")
				if peekNextChar == '"' {
					i++
				}
			}
		} else if char == '*' {
			builder.WriteString(".*?")
			isRegex = true
		} else if char == '?' {
			builder.WriteString(".{1}")
			isRegex = true
		} else if char == '+' {
			builder.WriteString(".+?")
			isRegex = true
		} else if unicode.IsLetter(char) || unicode.IsNumber(char) || char == '_' || isQuoted {
			builder.WriteRune(char)
		} else {
			return "", false, errors.Errorf(
				"illegal character in pattern '%s' at index %d", token, i,
			)
		}
	}

	parsedToken := builder.String()
	if !isQuoted && !isRegex {
		uppercaseParsedToken := strings.ToUpper(parsedToken)
		for _, keyword := range reservedKeywords {
			if keyword == uppercaseParsedToken {
				return "", false, errors.Errorf(
					"an unquoted pattern cannot match a reserved keyword: %s", keyword,
				)
			}
		}
	}

	return parsedToken, isRegex, nil
}
