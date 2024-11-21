// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/google/syzkaller/pkg/ast"
	"github.com/google/syzkaller/pkg/tool"
	"github.com/google/syzkaller/sys/targets"
)

const sendmsg = "sendmsg"

type compileCommand struct {
	Arguments []string
	Directory string
	File      string
	Output    string
}

type output struct {
	stdout string
	stderr string
}

func main() {
	compilationDatabase := flag.String("compile_commands", "compile_commands.json", "path to compilation database")
	binary := flag.String("binary", "syz-declextract", "path to binary")
	outFile := flag.String("output", "out.txt", "output file")
	kernelDir := flag.String("kernel", "", "kernel directory")
	flag.Parse()
	if *kernelDir == "" {
		tool.Failf("path to kernel directory is required")
	}

	fileData, err := os.ReadFile(*compilationDatabase)
	if err != nil {
		tool.Fail(err)
	}

	var cmds []compileCommand
	if err := json.Unmarshal(fileData, &cmds); err != nil {
		tool.Fail(err)
	}

	outputs := make(chan output, len(cmds))
	files := make(chan string, len(cmds))
	for w := 0; w < runtime.NumCPU(); w++ {
		go worker(outputs, files, *binary, *compilationDatabase)
	}

	for _, v := range cmds {
		files <- v.File
	}

	var syscalls []*ast.Call
	var netlinks []*ast.Struct
	var includes []*ast.Include
	var typeDefs []*ast.TypeDef
	var resources []*ast.Resource
	syscallNames := readSyscallNames(filepath.Join(*kernelDir, "arch"))
	// Some syscalls have different names and entry points and thus need to be renamed.
	// e.g. SYSCALL_DEFINE1(setuid16, old_uid_t, uid) is referred to in the .tbl file with setuid.

	eh := ast.LoggingHandler
	for range cmds {
		out := <-outputs
		if out.stderr != "" {
			tool.Failf("%s", out.stderr)
		}
		parse := ast.Parse([]byte(out.stdout), "", eh)
		if parse == nil {
			fmt.Println(out.stdout)
			tool.Failf("parsing error")
		}
		for _, node := range parse.Nodes {
			switch node := node.(type) {
			case *ast.Call:
				syscalls = append(syscalls, renameSyscall(node, syscallNames)...)
			case *ast.Struct:
				netlinks = append(netlinks, node)
			case *ast.Include:
				includes = append(includes, node)
			case *ast.TypeDef:
				typeDefs = append(typeDefs, node)
			case *ast.Resource:
				resources = append(resources, node)
			case *ast.NewLine:
				continue
			}
		}
	}

	close(files)
	writeOutput(includes, syscalls, netlinks, typeDefs, resources, *outFile)
}

func writeOutput(includes []*ast.Include, syscalls []*ast.Call, netlinks []*ast.Struct, types []*ast.TypeDef,
	resources []*ast.Resource, outFile string) {
	slices.SortFunc(includes, func(a, b *ast.Include) int {
		return strings.Compare(a.File.Value, b.File.Value)
	})
	includes = slices.CompactFunc(includes, func(a, b *ast.Include) bool {
		return a.File.Value == b.File.Value
	})

	slices.SortFunc(syscalls, func(a, b *ast.Call) int {
		nameCmp := strings.Compare(a.Name.Name, b.Name.Name)
		if nameCmp != 0 {
			return nameCmp
		}
		if a.CallName == sendmsg {
			// For sendmsg, compare by the policy name: sendmsg(_, msg ptr[_, msghdr_macsec_auto[_, PolicyName]], _).
			return strings.Compare(a.Args[1].Type.Args[1].Args[1].Ident, b.Args[1].Type.Args[1].Args[1].Ident)
		}
		return slices.CompareFunc(a.Args, b.Args, func(a, b *ast.Field) int {
			// Ensure deterministic output. Some system calls have the same name but share different parameter names; this
			// guarantees that the compact function will always keep the same one.
			return strings.Compare(a.Name.Name, b.Name.Name)
		})
	})

	sendmsgNo := 0
	// Some commands are executed for multiple policies. Ensure that they don't get deleted by the following compact call.
	for _, node := range syscalls {
		if node.CallName == sendmsg {
			node.Name.Name += strconv.Itoa(sendmsgNo)
			sendmsgNo++
		}
	}

	syscalls = slices.CompactFunc(syscalls, func(a, b *ast.Call) bool {
		// We only compare the the system call names for cases where the same system call has different parameter names,
		// but share the same syzkaller type. NOTE:Change when we have better type extraction.
		return a.Name.Name == b.Name.Name
	})

	slices.SortFunc(netlinks, func(a, b *ast.Struct) int {
		return strings.Compare(a.Name.Name, b.Name.Name)
	})

	slices.SortFunc(resources, func(a, b *ast.Resource) int {
		return strings.Compare(a.Name.Name, b.Name.Name)
	})

	slices.SortFunc(types, func(a, b *ast.TypeDef) int {
		return strings.Compare(a.Name.Name, b.Name.Name)
	})

	autoGeneratedNotice := "# Code generated by syz-declextract. DO NOT EDIT.\n"
	commonKernelHeaders := "include <include/vdso/bits.h>\ninclude <include/linux/types.h>"
	var netlinkNames []string
	mmap2 := "_ = __NR_mmap2\n"
	eh := ast.LoggingHandler
	desc := ast.Parse([]byte(autoGeneratedNotice+commonKernelHeaders), "", eh)
	for _, node := range includes {
		desc.Nodes = append(desc.Nodes, node)
	}
	for _, node := range resources {
		desc.Nodes = append(desc.Nodes, node)
	}
	for _, node := range types {
		desc.Nodes = append(desc.Nodes, node)
	}
	usedNetlink := make(map[string]bool)
	for _, node := range syscalls {
		if node.CallName == sendmsg && len(node.Args[1].Type.Args) == 2 {
			policy := node.Args[1].Type.Args[1].Args[1].Ident
			usedNetlink[policy] = true
			_, isDefined := slices.BinarySearchFunc(netlinks, policy, func(a *ast.Struct, b string) int {
				return strings.Compare(a.Name.Name, b)
			})
			if !isDefined {
				continue
			}
		}
		desc.Nodes = append(desc.Nodes, node)
	}
	desc.Nodes = append(desc.Nodes, ast.Parse([]byte(mmap2), "", eh).Nodes...)
	for _, node := range netlinks {
		desc.Nodes = append(desc.Nodes, node)
		name := node.Name.Name
		if !usedNetlink[name] {
			netlinkNames = append(netlinkNames, name)
		}
	}
	for i, netlink := range netlinkNames {
		netlinkNames[i] = fmt.Sprintf("\tpolicy%v msghdr_auto[%v]\n", i, netlink)
	}
	netlinkUnion := `
type msghdr_auto[POLICY] msghdr_netlink[netlink_msg_t[autogenerated_netlink, genlmsghdr, POLICY]]
resource autogenerated_netlink[int16]
syz_genetlink_get_family_id$auto(name ptr[in, string], fd sock_nl_generic) autogenerated_netlink
sendmsg$autorun(fd sock_nl_generic, msg ptr[in, auto_union], f flags[send_flags])
auto_union [
` + strings.Join(netlinkNames, "") + "]"
	netlinkUnionParsed := ast.Parse([]byte(netlinkUnion), "", eh)
	if netlinkUnionParsed == nil {
		tool.Failf("parsing error")
	}
	desc.Nodes = append(desc.Nodes, netlinkUnionParsed.Nodes...)

	err := os.WriteFile(outFile, ast.Format(ast.Parse(ast.Format(desc), "", eh)), 0666)
	// New lines are added in the parsing step. This is why we need to Format (serialize the description), Parse, then
	// Format again.
	if err != nil {
		tool.Fail(err)
	}
}

func worker(outputs chan output, files chan string, binary, compilationDatabase string) {
	for file := range files {
		if !strings.HasSuffix(file, ".c") {
			outputs <- output{}
			continue
		}

		cmd := exec.Command(binary, "-p", compilationDatabase, file)
		stdout, err := cmd.Output()
		var stderr string
		if err != nil {
			var error *exec.ExitError
			if errors.As(err, &error) {
				if len(error.Stderr) != 0 {
					stderr = string(error.Stderr)
				} else {
					stderr = fmt.Sprintf("%v: %v", file, error.String())
				}
			} else {
				stderr = err.Error()
			}
		}
		outputs <- output{string(stdout), stderr}
	}
}

func renameSyscall(syscall *ast.Call, rename map[string][]string) []*ast.Call {
	if !shouldRenameSyscall(syscall.CallName) {
		return []*ast.Call{syscall}
	}
	var renamed []*ast.Call
	toReplace := syscall.CallName
	if rename[toReplace] == nil {
		// Syscall has no record in the tables for the architectures we support.
		return nil
	}

	for _, name := range rename[toReplace] {
		if isProhibited(name) {
			continue
		}
		newCall := syscall.Clone().(*ast.Call)
		newCall.Name.Name = name + "$auto"
		newCall.CallName = name // Not required	but avoids mistakenly treating CallName as the part before the $.
		renamed = append(renamed, newCall)
	}

	return renamed
}

func readSyscallNames(kernelDir string) map[string][]string {
	var rename = make(map[string][]string)
	for _, arch := range targets.List[targets.Linux] {
		filepath.Walk(filepath.Join(kernelDir, arch.KernelHeaderArch),
			func(path string, info fs.FileInfo, err error) error {
				if !strings.HasSuffix(path, ".tbl") {
					return nil
				}
				fi, fErr := os.Lstat(path)
				if fErr != nil {
					tool.Fail(err)
				}
				if fi.Mode()&fs.ModeSymlink != 0 { // Some symlinks link to files outside of arch directory.
					return nil
				}
				f, fErr := os.Open(path)
				if fErr != nil {
					tool.Fail(err)
				}
				s := bufio.NewScanner(f)
				for s.Scan() {
					fields := strings.Fields(s.Text())
					if len(fields) < 4 || fields[0] == "#" || strings.HasPrefix(fields[2], "unused") || fields[3] == "-" ||
						strings.HasPrefix(fields[3], "compat") || strings.HasPrefix(fields[3], "sys_ia32") ||
						fields[3] == "sys_ni_syscall" {
						// System calls prefixed with ia32 are ignored due to conflicting system calls for 64 bit and 32 bit.
						continue
					}
					key := strings.TrimPrefix(fields[3], "sys_")
					rename[key] = append(rename[key], fields[2])
				}
				return nil
			})
	}

	for k := range rename {
		slices.Sort(rename[k])
		rename[k] = slices.Compact(rename[k])
	}

	return rename
}

func shouldRenameSyscall(syscall string) bool {
	switch syscall {
	case sendmsg, "syz_genetlink_get_family_id":
		return false
	default:
		return true
	}
}

func isProhibited(syscall string) bool {
	switch syscall {
	case "reboot", "utimesat": // `utimesat` is not defined for all arches.
		return true
	default:
		return false
	}
}
