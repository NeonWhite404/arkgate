package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestWebTemplatesTagBalanced 检查前端模板里的 div 标签配平。
//
// 为什么需要这个测试：`web/*.js` 的模板是**字符串里的 HTML**，没有构建链
// （无 Vite/Node）也没有编译器前端校验。多一个 `</div>` 不会在 Go 侧报错，
// 只在浏览器里炸——而炸出来的错误极其难定位。
//
// 实测代价：给子 Key 抽屉加「缓存计权倍率」段时多写了一个 `</div>`，
// 结果**整个子 Key 页面白屏**，控制台报的是 Vue 编译器内部的
// `Cannot read properties of undefined (reading 'type')`
// （模板编译期的标签栈不匹配），完全看不出是标签问题。花了不少时间
// 才用逐行二分定位到那一行。
//
// 这个测试用「标签计数」把问题挡在最便宜的地方。它不能替代浏览器验证
// （属性/插值错误查不出来），但能挡住「漏/多写一个 div」这类高频错误。
//
// **实现要点：必须按标签计数而不是按行计数。** 模板里存在跨行的开标签
// （属性太多折行），例如：
//
//	<div v-for="it in items" :key="it.name" class="split-seg"
//	     :style="{width: it.pct + '%'}"></div>
//
// 这种写法下「同一行有 </div> 但没有 <div>」是**合法的**，按行统计会产生
// 假阳性（初版就因此把一个正确的模板报成不平衡，白花时间排查）。
func TestWebTemplatesTagBalanced(t *testing.T) {
	files := []string{"web/app.js", "web/models.js", "web/ui.js", "web/index.html"}
	// 只关心 div —— 它占了模板标签的绝大多数，且是最容易配错的那个。
	// 同样配平的还有 span/template/table 系，但那些嵌套少、出错概率低，
	// 全量实现会引入 <br>/<img> 等自闭合标签的复杂度，不划算。
	//
	// 自闭合 `<div ... />` 在本项目的模板里不出现，故不特判。
	tagRe := regexp.MustCompile(`<(/?)div\b[^>]*?(/?)>`)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s: %v", f, err)
		}
		src := stripTemplateLiterals(string(raw))

		// 逐**标签**累加（不是逐行），用下标定位到出错位置的行号，
		// 便于直接跳过去改。
		depth := 0
		for _, m := range tagRe.FindAllStringSubmatchIndex(src, -1) {
			closing := src[m[2]:m[3]] == "/"
			selfClosing := m[4] >= 0 && m[5] > m[4] // `<div ... />`
			if selfClosing {
				continue
			}
			if closing {
				depth--
				if depth < 0 {
					t.Errorf("%s:%d 处 </div> 多于 <div>——多半是多余的闭合标签",
						f, lineOf(src, m[0]))
					return
				}
			} else {
				depth++
			}
		}
		if depth != 0 {
			t.Errorf("%s 的 div 标签未配平：<div> 比 </div> 多 %d 个（漏了闭合标签）", f, depth)
		}
	}
}

// stripTemplateLiterals 去掉 JS 源码里的模板字符串定界符之外的干扰项。
//
// 只保留反引号内部的内容——模板都在反引号里，而普通的 JS 代码里
// 出现 `<div` 字面量的地方（比如注释举例子）不该参与配平。
func stripTemplateLiterals(src string) string {
	var sb strings.Builder
	inTpl := false
	for _, ln := range strings.Split(src, "\n") {
		// 统计该行的反引号数量（模板里不出现转义反引号，够用）。
		// 一行开一个模板、同一行又闭掉的情况（单行模板）也要算上。
		for i := 0; i < len(ln); i++ {
			if ln[i] == '`' {
				inTpl = !inTpl
				continue
			}
			if inTpl {
				sb.WriteByte(ln[i])
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// lineOf 返回字节偏移对应的 1-based 行号。
func lineOf(src string, off int) int {
	if off > len(src) {
		off = len(src)
	}
	return strings.Count(src[:off], "\n") + 1
}
