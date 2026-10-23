package upstream

import "sort"

// sortStrings 是 sort.Strings 的短别名，避免在多个文件重复 import sort。
func sortStrings(values []string) { sort.Strings(values) }
