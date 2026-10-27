package remote

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

const itemCountHeader = "Lode-Item-Count"
const cleanupTokenHeader = "Lode-Cleanup-Token"
const listContentType = "application/x-ndjson"

// Lists have a bounded record size, not a history-sized response limit. The
// count lets readers distinguish a complete empty list from an interrupted
// response. Project methods supply one coherent result before streaming begins.
func writeList[T any](w http.ResponseWriter, values []T) {
	w.Header().Set("Content-Type", listContentType)
	w.Header().Set(itemCountHeader, strconv.Itoa(len(values)))
	encoder := json.NewEncoder(w)
	for _, value := range values {
		if err := encoder.Encode(value); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func readList[T any](c *Client, resp *http.Response, visit func(T) error) error {
	defer resp.Body.Close()
	if err := c.verify(resp); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return parseError(resp)
	}
	count, err := strconv.Atoi(resp.Header.Get(itemCountHeader))
	if err != nil || count < 0 || resp.Header.Get("Content-Type") != listContentType {
		return errors.New("invalid remote list header")
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), maxMetadata+1)
	seen := 0
	for scanner.Scan() {
		if seen == count {
			return errors.New("remote list contains extra records")
		}
		var value T
		if err := decodeMetadata(bytes.NewReader(scanner.Bytes()), &value); err != nil {
			return fmt.Errorf("invalid remote list record: %w", err)
		}
		if visit != nil {
			if err := visit(value); err != nil {
				return err
			}
		}
		seen++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read remote list (records must fit in 16 MiB): %w", err)
	}
	if seen != count {
		return fmt.Errorf("incomplete remote list: received %d of %d records", seen, count)
	}
	return nil
}

func collectList[T any](c *Client, resp *http.Response) ([]T, error) {
	values := []T{}
	err := readList(c, resp, func(value T) error {
		values = append(values, value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}
