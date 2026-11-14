// @author: kevinkelly
// @license: Apache License 2.0
// @file: regionkey.go
// @time: 2026/10/28

package collect

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 热点 Region 的范围展示：PD 返回的 start_key / end_key 是十六进制编码的「TiKV 内部 Key」。
// TiDB 的 Key（t{tableID}_r{rowID} / t{tableID}_i{indexID}…）在 TiKV 里还要再过一遍 memcomparable 字节编码
// （每 8 字节一组、后跟 1 字节标记）。这里只做尽力而为的解码：能解出表 / 行 / 索引就给人话，否则回退到截断的十六进制。

// RegionRange 把 PD 返回的 start_key / end_key（十六进制）渲染成人类可读的范围。
// 两端都空表示整个 Key 空间，返回空串（调用方通常不需要展示）。
func RegionRange(startHex, endHex string) string {
	startHex, endHex = strings.TrimSpace(startHex), strings.TrimSpace(endHex)
	if startHex == "" && endHex == "" {
		return ""
	}
	s, sok := decodeTiDBKey(startHex)
	e, eok := decodeTiDBKey(endHex)
	if sok && eok {
		return joinKeyRange(s, e)
	}
	return hexShort(startHex, "-∞") + " → " + hexShort(endHex, "+∞")
}

// hexShort 截断过长的十六进制：保留前后各 8 个字符。
func hexShort(h, empty string) string {
	if h == "" {
		return empty
	}
	if len(h) <= 20 {
		return strings.ToUpper(h)
	}
	return strings.ToUpper(h[:8]) + "…" + strings.ToUpper(h[len(h)-8:])
}

// tidbKey 是解码后的 TiDB Key。
type tidbKey struct {
	empty   bool  // 空 Key（Key 空间的起点 / 终点）
	meta    bool  // m 前缀：TiDB 元数据
	table   int64 // 表 ID（物理表 / 分区 ID）
	hasTbl  bool  // table 有效
	kind    byte  // 0 仅表前缀 | 'r' 行 | 'i' 索引
	id      int64 // 行：RowID；索引：IndexID
	hasID   bool  // id 有效
	complex bool  // 行键的 handle 不是 8 字节整数（聚簇索引 / 非整数主键）
}

// decodeTiDBKey 解码一个十六进制 Key。解码不出 TiDB 的 t / m 前缀时返回 false。
func decodeTiDBKey(h string) (tidbKey, bool) {
	if h == "" {
		return tidbKey{empty: true}, true
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) == 0 {
		return tidbKey{}, false
	}
	// 先按 memcomparable 解码；失败再把它当作未编码的原始 Key（部分版本 / 场景下 PD 返回原始 Key）
	if raw, ok := decodeMemcmp(b); ok && len(raw) > 0 && (raw[0] == 't' || raw[0] == 'm') {
		if k, ok := parseTiDBKey(raw); ok {
			return k, true
		}
	}
	if b[0] == 't' || b[0] == 'm' {
		return parseTiDBKey(b)
	}
	return tidbKey{}, false
}

// decodeMemcmp 解码 TiKV 的 memcomparable 字节编码：每组 8 字节数据 + 1 字节标记（0xFF - 填充字节数）。
// 最后不足 9 字节的残缺分组（Key 被截断时常见）按原样保留，不视为失败。
func decodeMemcmp(b []byte) ([]byte, bool) {
	var out []byte
	for len(b) > 0 {
		if len(b) < 9 {
			return append(out, b...), true
		}
		marker := b[8]
		if marker < 0xF7 {
			return nil, false
		}
		pad := int(0xFF - marker)
		out = append(out, b[:8-pad]...)
		if pad > 0 {
			for _, c := range b[8-pad : 8] {
				if c != 0 {
					return nil, false
				}
			}
			return out, true // 带填充的分组一定是最后一组
		}
		b = b[9:]
	}
	return out, true
}

func decodeCmpInt(b []byte) int64 {
	return int64(binary.BigEndian.Uint64(b) ^ (1 << 63))
}

// parseTiDBKey 解析原始 TiDB Key：t{tableID}[_r{rowID} | _i{indexID}{values}]，或 m 开头的元数据 Key。
func parseTiDBKey(raw []byte) (tidbKey, bool) {
	if len(raw) == 0 {
		return tidbKey{}, false
	}
	if raw[0] == 'm' {
		return tidbKey{meta: true}, true
	}
	if raw[0] != 't' || len(raw) < 9 {
		return tidbKey{}, false
	}
	k := tidbKey{table: decodeCmpInt(raw[1:9]), hasTbl: true}
	rest := raw[9:]
	if len(rest) >= 2 && rest[0] == '_' && (rest[1] == 'r' || rest[1] == 'i') {
		k.kind = rest[1]
		rest = rest[2:]
		switch {
		case k.kind == 'r' && len(rest) == 8:
			k.id, k.hasID = decodeCmpInt(rest), true
		case k.kind == 'r' && len(rest) > 8:
			k.complex = true
		case k.kind == 'i' && len(rest) >= 8:
			k.id, k.hasID = decodeCmpInt(rest[:8]), true
		}
	}
	return k, true
}

// describe 渲染一个 Key。withTable 为 false 时省略 "table_id=…" 前缀（同一张表的范围终点）。
func (k tidbKey) describe(withTable bool) string {
	switch {
	case k.empty:
		return ""
	case k.meta:
		return "TiDB 元数据"
	}
	var parts []string
	if withTable {
		parts = append(parts, "table_id="+strconv.FormatInt(k.table, 10))
	}
	switch {
	case k.kind == 'r' && k.hasID:
		parts = append(parts, "row "+strconv.FormatInt(k.id, 10))
	case k.kind == 'r' && k.complex:
		parts = append(parts, "row（聚簇索引键）")
	case k.kind == 'r':
		parts = append(parts, "row")
	case k.kind == 'i' && k.hasID:
		parts = append(parts, "index "+strconv.FormatInt(k.id, 10))
	case k.kind == 'i':
		parts = append(parts, "index")
	}
	return strings.Join(parts, " ")
}

func joinKeyRange(s, e tidbKey) string {
	ss, es := s.describe(true), e.describe(true)
	if s.empty {
		ss = "-∞"
	}
	if e.empty {
		es = "+∞"
	}
	if s.hasTbl && e.hasTbl && s.table == e.table {
		// 同一个索引的范围只显示一份；同一张表内终点省略表前缀
		if s.kind == 'i' && e.kind == 'i' && s.hasID && e.hasID && s.id == e.id {
			return ss
		}
		if d := e.describe(false); d != "" {
			es = d
		}
	}
	return ss + " … " + es
}

// ---------------------------------------------------------------- PD 响应里的 Key ----

// ParseRegionKeys 解析 PD 的 Region 列表（/regions、/regions/writeflow、/regions/readflow 的响应）。
func ParseRegionKeys(b []byte) (map[uint64][2]string, error) {
	var r struct {
		Regions []struct {
			ID       uint64 `json:"id"`
			StartKey string `json:"start_key"`
			EndKey   string `json:"end_key"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("解析 Region 列表失败: %w", err)
	}
	out := map[uint64][2]string{}
	for _, x := range r.Regions {
		out[x.ID] = [2]string{x.StartKey, x.EndKey}
	}
	return out, nil
}
