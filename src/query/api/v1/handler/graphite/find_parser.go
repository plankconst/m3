// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package graphite

import (
	"fmt"
	"io"
	"net/http"

	"github.com/m3db/m3/src/query/errors"
	graphiteStorage "github.com/m3db/m3/src/query/graphite/storage"
	"github.com/m3db/m3/src/query/storage"
	"github.com/m3db/m3/src/query/util/json"
	"github.com/m3db/m3/src/x/net/http"
)

func parseFindParamsToQuery(r *http.Request) (
	*storage.CompleteTagsQuery,
	string,
	*xhttp.ParseError,
) {
	values := r.URL.Query()
	query := values.Get("query")
	if query == "" {
		return nil, "", xhttp.NewParseError(errors.ErrNoQueryFound, http.StatusBadRequest)
	}

	matchers, err := graphiteStorage.TranslateQueryToMatchers(query)
	if err != nil {
		return nil, "", xhttp.NewParseError(fmt.Errorf("invalid 'query': %s", query),
			http.StatusBadRequest)
	}

	return &storage.CompleteTagsQuery{
		CompleteNameOnly: false,
		TagMatchers:      matchers,
	}, query, nil
}

func findResultsJSON(
	w io.Writer,
	prefix string,
	tags map[string]bool,
) error {
	jw := json.NewWriter(w)
	jw.BeginArray()

	for value, hasChildren := range tags {
		leaf := 1
		if hasChildren {
			leaf = 0
		}
		jw.BeginObject()

		jw.BeginObjectField("id")
		jw.WriteString(fmt.Sprintf("%s%s", prefix, value))

		jw.BeginObjectField("text")
		jw.WriteString(value)

		jw.BeginObjectField("leaf")
		jw.WriteInt(leaf)

		jw.BeginObjectField("expandable")
		jw.WriteInt(1 - leaf)

		jw.BeginObjectField("allowChildren")
		jw.WriteInt(1 - leaf)

		jw.EndObject()
	}

	jw.EndArray()
	return jw.Close()
}
