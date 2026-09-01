package vmwareCollectors

import (
	"flag"
	"testing"
)

func TestDefinitionsAreSortedAndComplete(t *testing.T) {
	defs := Definitions()

	if len(defs) == 0 {
		t.Fatal("Definitions() returned an empty list")
	}

	for i := 1; i < len(defs); i++ {
		if defs[i-1].Name >= defs[i].Name {
			t.Fatalf("Definitions() is not sorted: %q comes before %q", defs[i-1].Name, defs[i].Name)
		}
	}

	for _, d := range defs {
		if d.Name == "" {
			t.Fatal("found a definition with an empty Name")
		}

		if d.Creator == nil {
			t.Fatalf("collector %q has a nil Creator", d.Name)
		}
	}
}

// TestDefinitionsReturnsCopy 确认调用方拿到的是副本，
// 改动返回值不会污染全局清单。
func TestDefinitionsReturnsCopy(t *testing.T) {
	first := Definitions()
	original := first[0].Name

	first[0].Name = "mutated"

	second := Definitions()

	if second[0].Name != original {
		t.Fatalf("Definitions() leaked internal state: got %q after mutation, want %q", second[0].Name, original)
	}
}

// TestDefinitionsMatchRegisteredFlags 保证清单中每个 collector 都注册了
// 对应的 -collector.<name> 开关。
//
// 清单驱动 /probe，命令行开关驱动 /metrics。框架的 collectorState 是私有的，
// 无法反查，所以两边只能靠这个测试保持同步：新增 collector 时若只改了一边，
// 这里就会失败。
func TestDefinitionsMatchRegisteredFlags(t *testing.T) {
	for _, name := range Names() {
		flagName := "collector." + name

		if flag.Lookup(flagName) == nil {
			t.Fatalf("collector %q is listed in Definitions() but -%s is not registered; /metrics and /probe would diverge",
				name, flagName)
		}
	}
}

func TestNamesMatchDefinitions(t *testing.T) {
	defs := Definitions()
	names := Names()

	if len(names) != len(defs) {
		t.Fatalf("Names() returned %d entries, Definitions() has %d", len(names), len(defs))
	}

	for i := range defs {
		if names[i] != defs[i].Name {
			t.Fatalf("Names()[%d] = %q, want %q", i, names[i], defs[i].Name)
		}
	}
}

// TestCreatorsProduceCollectors 确认每个 Creator 都能真正构造出实例。
// 清单里挂错函数或返回 nil 会让对应 collector 在运行期静默失效。
func TestCreatorsProduceCollectors(t *testing.T) {
	for _, d := range Definitions() {
		t.Run(d.Name, func(t *testing.T) {
			instance, err := d.Creator(nil)
			if err != nil {
				t.Fatalf("Creator() returned error: %v", err)
			}

			if instance == nil {
				t.Fatal("Creator() returned a nil collector")
			}
		})
	}
}
