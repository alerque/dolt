// Copyright 2023 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package doltdb

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/durable"
	"github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
)

type ignorePattern struct {
	pattern string
	ignore  bool
}

type IgnorePatterns []ignorePattern

func GetIgnoredTablePatterns(ctx context.Context, roots Roots) (IgnorePatterns, error) {
	var ignorePatterns []ignorePattern
	workingSet := roots.Working
	table, found, err := workingSet.GetTable(ctx, IgnoreTableName)
	if err != nil {
		return nil, err
	}
	if !found {
		// dolt_ignore doesn't exist, so don't filter any tables.
		return ignorePatterns, nil
	}
	index, err := table.GetRowData(ctx)
	if table.Format() == types.Format_LD_1 {
		// dolt_ignore is not supported for the legacy storage format.
		return ignorePatterns, nil
	}
	if err != nil {
		return nil, err
	}
	ignoreTableSchema, err := table.GetSchema(ctx)
	if err != nil {
		return nil, err
	}
	keyDesc, valueDesc := ignoreTableSchema.GetMapDescriptors()

	if !keyDesc.Equals(val.NewTupleDescriptor(val.Type{Enc: val.StringEnc})) {
		return nil, fmt.Errorf("dolt_ignore had unexpected key type, this should never happen")
	}
	if !valueDesc.Equals(val.NewTupleDescriptor(val.Type{Enc: val.Int8Enc, Nullable: true})) {
		return nil, fmt.Errorf("dolt_ignore had unexpected value type, this should never happen")
	}

	ignoreTableMap, err := durable.ProllyMapFromIndex(index).IterAll(ctx)
	if err != nil {
		return nil, err
	}
	for {
		keyTuple, valueTuple, err := ignoreTableMap.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		pattern, ok := keyDesc.GetString(0, keyTuple)
		if !ok {
			return nil, fmt.Errorf("could not read pattern")
		}
		ignore, ok := valueDesc.GetBool(0, valueTuple)
		ignorePatterns = append(ignorePatterns, ignorePattern{pattern, ignore})
	}
	return ignorePatterns, nil
}

// compilePattern takes a dolt_ignore pattern and generate a Regexp that matches against the same table names as the pattern.
func compilePattern(pattern string) (*regexp.Regexp, error) {
	pattern = "^" + regexp.QuoteMeta(pattern) + "$"
	pattern = strings.Replace(pattern, "\\?", ".", -1)
	pattern = strings.Replace(pattern, "\\*", ".*", -1)
	return regexp.Compile(pattern)
}

// getMoreSpecificPatterns takes a dolt_ignore pattern and generates a Regexp that matches against all patterns
// that are "more specific" than it. (a pattern A is more specific than a pattern B if all names that match A also
// match pattern B, but not vice versa.)
func getMoreSpecificPatterns(lessSpecific string) (*regexp.Regexp, error) {
	pattern := "^" + regexp.QuoteMeta(lessSpecific) + "$"
	// A ? can expand to any character except for a *, since that also has special meaning in patterns.
	pattern = strings.Replace(pattern, "\\?", "[^\\*]", -1)
	pattern = strings.Replace(pattern, "\\*", ".*", -1)
	return regexp.Compile(pattern)
}

func resolveConflictingPatterns(trueMatches, falseMatches []string, tableName string) (bool, error) {
	trueMatchesToRemove := map[string]struct{}{}
	falseMatchesToRemove := map[string]struct{}{}
	for _, trueMatch := range trueMatches {
		trueMatchRegExp, err := getMoreSpecificPatterns(trueMatch)
		if err != nil {
			return false, err
		}
		for _, falseMatch := range falseMatches {
			if trueMatchRegExp.MatchString(falseMatch) {
				trueMatchesToRemove[trueMatch] = struct{}{}
			}
		}
	}
	for _, falseMatch := range falseMatches {
		falseMatchRegExp, err := getMoreSpecificPatterns(falseMatch)
		if err != nil {
			return false, err
		}
		for _, trueMatch := range trueMatches {
			if falseMatchRegExp.MatchString(trueMatch) {
				falseMatchesToRemove[falseMatch] = struct{}{}
			}
		}
	}
	if len(trueMatchesToRemove) == len(trueMatches) {
		return false, nil
	}
	if len(falseMatchesToRemove) == len(falseMatches) {
		return true, nil
	}

	// There's a conflict. Remove the less specific patterns so that only the conflict remains.

	var conflictingTrueMatches []string
	var conflictingFalseMatches []string

	for _, trueMatch := range trueMatches {
		if _, ok := trueMatchesToRemove[trueMatch]; !ok {
			conflictingTrueMatches = append(conflictingTrueMatches, trueMatch)
		}
	}

	for _, falseMatch := range falseMatches {
		if _, ok := trueMatchesToRemove[falseMatch]; !ok {
			conflictingFalseMatches = append(conflictingFalseMatches, falseMatch)
		}
	}

	return false, DoltIgnoreConflict{Table: tableName, TruePatterns: conflictingTrueMatches, FalsePatterns: conflictingFalseMatches}
}

func (ip *IgnorePatterns) IsTableNameIgnored(tableName string) (bool, error) {
	trueMatches := []string{}
	falseMatches := []string{}
	for _, patternIgnore := range *ip {
		pattern := patternIgnore.pattern
		ignore := patternIgnore.ignore
		patternRegExp, err := compilePattern(pattern)
		if err != nil {
			return false, err
		}
		if patternRegExp.MatchString(tableName) {
			if ignore {
				trueMatches = append(trueMatches, pattern)
			} else {
				falseMatches = append(falseMatches, pattern)
			}
		}
	}
	if len(trueMatches) == 0 {
		return false, nil
	}
	if len(falseMatches) == 0 {
		return true, nil
	}
	// The table name matched both positive and negative patterns.
	// More specific patterns override less specific patterns.
	ignoreTable, err := resolveConflictingPatterns(trueMatches, falseMatches, tableName)
	if err != nil {
		return false, err
	}
	return ignoreTable, nil
}