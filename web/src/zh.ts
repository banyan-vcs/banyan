// zh.ts — the Chinese dictionary. The key is the English source text
// itself (English is the fallback and needs no dictionary); values may
// reorder positional {n} placeholders, which is the whole point of
// positional over named. i18n.test.tsx pins placeholder parity between
// each key and its value. Grouped by file, roughly in route order.
export const zh: Record<string, string> = {
  // ui.tsx
  "loading…": "加载中…",
  "something went wrong: {0}": "出问题了：{0}",

  // Layout.tsx — menus and headings wear Title Case (acronyms keep their
  // caps: SSH); labels, buttons and table columns stay sentence case.
  "Repositories": "仓库",
  "Groups": "分组",
  "Merge Requests": "合并请求",
  "Overview": "概览",
  overview: "概览",
  "Blame": "追溯",
  "Code": "代码",
  "Commits": "提交",
  "Branches": "分支",
  "Subtrees": "子树",
  "Members": "成员",
  "General": "基本设置",
  "Settings": "设置",
  "Profile": "个人信息",
  "SSH Keys": "SSH 密钥",
  "Access Tokens": "访问令牌",
  repositories: "仓库",
  group: "分组",
  branches: "分支",
  "expand the sidebar ( [ )": "展开侧栏（ [ ）",
  "fold to icons ( [ )": "折叠为图标（ [ ）",
  "expand sidebar": "展开侧栏",
  "collapse sidebar": "折叠侧栏",
  "banyan · the tree becomes a forest": "banyan · 独木成林",

  // UserMenu.tsx
  "{0} — Settings & Log Out": "{0} —— 设置与退出",
  "logging out…": "正在退出…",
  "Log Out": "退出登录",

  // NewMenu.tsx
  "Create": "创建",
  create: "创建",
  "new group": "新建分组",
  "New Repository": "新建仓库",
  "New Group": "新建分组",

  // LoginPage.tsx
  "Banyan Console": "banyan 控制台",
  "every page shows the repositories and branches this account can see — the same view git gets.":
    "每个页面展示该账号可见的仓库与分支——与 git 看到的完全一致。",
  "wrong user or password": "用户名或密码错误",
  user: "用户名",
  password: "密码",
  "checking…": "正在验证…",
  "log in": "登录",
  "continue with {0}": "使用 {0} 继续",

  // ReposPage.tsx
  repository: "仓库",
  description: "描述",
  updated: "最近更新",
  "no repositories yet — push one (first push auto-creates) or use the {0} in the top bar.":
    "还没有仓库——push 一个（首次 push 自动创建），或点顶栏的 {0}。",

  // GroupsPage.tsx
  "a group is an organizational container: it folders the repository list and gates nothing — names and URLs stay untouched. Repositories take the label at creation or from their settings page.":
    "分组只是组织容器：为仓库列表分拣目录、不做任何门禁——名称与 URL 一概不动。仓库在创建时或设置页挂上标签。",
  "a group is an organizational container: it owns no code and gates nothing — it folders the repository list and nothing else. Repositories take the label at creation or from their settings page.":
    "分组只是组织容器：不拥有代码、不做任何门禁——只为仓库列表分拣目录而已。仓库在创建时或设置页挂上标签。",
  "no groups yet.": "还没有分组。",
  created: "创建时间",
  "delete group {0}? its repositories keep their names — the label just goes away.":
    "删除分组 {0}？其中的仓库保留原名——只是标签消失。",
  delete: "删除",

  // merge request pages (states double as tab labels)
  open: "开启",
  merged: "已合并",
  closed: "已关闭",
  all: "全部",
  request: "请求",
  state: "状态",
  author: "作者",
  "nothing here — requests opened from a repository's page land in this list too.":
    "这里空空如也——从仓库页面发起的请求也会进入此列表。",
  "new merge request": "新建合并请求",
  "source, target and title are required": "源分支、目标分支与标题必填",
  "source branch…": "源分支…",
  "target branch…": "目标分支…",
  title: "标题",
  "open request": "创建",
  "no {0} merge requests.": "没有{0}的合并请求。",
  "URLs are /r/{0}/-/merge_requests/<iid> — IIDs never restart, merged and closed requests keep their links.":
    "URL 形如 /r/{0}/-/merge_requests/<iid>——IID 永不重置，已合并与已关闭的请求链接永久有效。",

  // MergeRequestPage.tsx
  "merge commit message (empty = default)": "合并提交信息（留空 = 默认）",
  "no merge request !{0} in {1} — IIDs are never reused, so this link points at nothing.":
    "仓库 {1} 中不存在合并请求 !{0}——IID 永不复用，此链接不指向任何东西。",
  "opened by {0}": "由 {0} 开启",
  "merged as {0} by {1}": "已由 {1} 合并为 {0}",
  changes: "变更",
  "the request is {0} — the change view is the stored record above.":
    "该请求状态为 {0}——变更视图是上方留存的历史记录。",
  "unrelated histories — no merge base to diff against.": "历史无关——没有可对照的合并基。",
  split: "分栏",
  unified: "单栏",
  "{0} file(s) changed": "{0} 个文件变更",
  "{0} hunk(s)": "{0} 个块",
  binary: "二进制",
  deleted: "已删除",
  "no changes": "无变更",
  "loading editor…": "正在加载编辑器…",
  "merge status": "合并状态",
  approvals: "审批",
  "mergeable — no conflicts against {0}.": "可合并——与 {0} 无冲突。",
  "conflicts: ": "冲突：",
  "the branches conflict": "分支间存在冲突",
  " — resolve on the source side, the check is live.": "——请在源分支解决，检查是实时的。",
  "target rule: ": "目标分支规则：",
  " — needs {0} approval(s) from others": "——还需他人 {0} 票审批",
  merge: "合并",
  "revoke my approval": "撤回我的审批",
  approve: "批准",
  close: "关闭",
  reopen: "重新开启",
  "none yet": "暂无",
  " — {0} needed": "——还需 {0} 票",
  "{0}/{1} — the author's own approval never counts.": "{0}/{1}——作者本人的审批永不计入。",

  // NewPage.tsx
  "Not in This Deployment": "此部署未提供",
  "this page is a standalone-console capability; this deployment (profile {0}) does not expose it.":
    "本页属于独立控制台能力；当前部署（profile {0}）未开放。",
  blank: "空白",
  "import from URL": "从 URL 导入",
  "a new repository at {0} — the group folders the list and the address, the name stays a single flat segment underneath (the git URL keeps speaking it).":
    "新仓库位于 {0}——分组决定列表与地址的目录，名称是其下单层平段（git URL 仍直接使用它）。",
  "groups unavailable on this deployment — creation needs one.": "当前部署没有分组可用——创建仓库需要先有分组。",
  "one URL, one credential: github.com, gitlab.com or a private GitLab — the gateway fetches every ref server-side and the imported repo is a first-class banyan repository. Local passwords are never sent to the source host.":
    "一个 URL、一份凭据：github.com、gitlab.com 或私有 GitLab——网关在服务端抓取全部 ref，导入的仓库是正经的 banyan 一等仓库。本地密码永不发往源站。",
  "Import History": "导入历史",
  "the repository lives at /<group>/<name>": "仓库位于 /<group>/<name>",
  "group…": "分组…",
  "repository name (single segment)": "仓库名（单段）",
  "description (shown in the repository list)": "描述（展示在仓库列表）",
  "nest under a parent project": "嵌套到某个父项目下",
  "— root project —": "— 根项目 —",
  "no groups yet — {0}; every repository lives under a group.": "还没有分组——先{0}；每个仓库都归属于某个分组。",
  "create one first": "创建一个",
  "https://github.com/owner/repo (or any git http URL)": "https://github.com/owner/repo（或任意 git http URL）",
  "my {0} login": "我的 {0} 登录",
  "private token": "私有令牌",
  "github / gitlab token": "github / gitlab 令牌",
  "target name": "目标名称",
  import: "导入",
  "starting…": "正在启动…",
  source: "来源",
  refs: "ref",
  status: "状态",
  started: "开始时间",
  "nothing imported yet.": "还没有导入记录。",
  running: "运行中",
  done: "完成",
  failed: "失败",
  "group name (single segment)": "分组名（单段）",
  "what belongs here (optional)": "这里放什么（可选）",
  "create group": "创建分组",

  // RepoHomePage.tsx
  "default: {0}": "默认：{0}",
  "Branches · Your View": "分支 · 你的视图",
  "no branches yet — push one.": "还没有分支——push 一个吧。",
  branch: "分支",
  head: "头提交",
  default: "默认",
  "personal branches of other users are simply not advertised here — the same filtered ref list your git client gets.":
    "其他用户的个人分支在这里就是不可见——与你 git 客户端拿到的过滤后 ref 列表一致。",
  "Recent Commits": "最近提交",
  "no history yet.": "还没有提交。",
  "all commits →": "全部提交 →",
  "Sub-Projects": "子项目",
  "this project has no sub-projects yet.": "该项目还没有子项目。",
  "sub-project": "子项目",
  "default branch": "默认分支",
  "new sub-project under this repo": "在此仓库下新建子项目",
  "create sub-project": "创建子项目",

  // TreePage.tsx
  "(default)": "（默认）",
  name: "名称",
  size: "大小",
  submodule: "子模块",
  blame: "追溯",
  "resolving…": "正在解析…",
  "loading file…": "正在加载文件…",

  // HistoryPage.tsx
  "filter by path (e.g. /a/b — only commits touching that subtree)":
    "按路径过滤（如 /a/b——只看触及该子树的提交）",
  filter: "过滤",
  "no commits": "暂无提交",
  " touching {0}": "（触及 {0}）",
  "load more": "加载更多",

  // BranchesPage.tsx
  "branch names are single-segment in banyan (the timeline model) — no slashes":
    "banyan 的分支名是单段的（时间线模型）——不能带斜杠",
  "new branch": "新分支",
  "from {0}": "基于 {0}",
  "no source branch to branch from": "没有可作源的分支",
  "branch vanished": "分支已不存在",
  "delete branch {0}?": "删除分支 {0}？",
  "creation and deletion go through the same ref CAS (old→new) a git push rides.":
    "创建与删除走的是 git push 同款的 ref CAS（old→new）。",

  // CommitPage.tsx
  "commit not found.": "找不到该提交。",
  parent: "父提交",
  "merge ({0} parents)": "合并（{0} 个父提交）",
  "root commit — no parent to diff against.": "根提交——没有可对照的父提交。",
  "diffing…": "正在计算 diff…",
  "← commits": "← 提交",

  // BlamePage.tsx
  "← back to the file": "← 返回文件",

  // SubtreesPage.tsx
  "every directory that owns a commit history. {0} marks the projection refs (refs/banyan/tree/<dir>) — one URL, one credential per subtree.":
    "每个目录都可以拥有自己的提交历史。{0} 标记投影 ref（refs/banyan/tree/<dir>）——每个子树一个 URL、一份凭据。",
  "no directory timelines yet — push into a subdirectory.": "还没有目录时间线——向某个子目录 push 即可。",
  path: "路径",
  projected: "投影",
  restricted: "受限",
  "select a subtree to see its activity": "选择一个子树查看其活动",
  " and access rule": "与访问规则",
  "Branch Heads": "分支头",
  clone: "克隆",
  activity: "活动",
  "via the GitLab URL (one URL, one credential):": "经 GitLab URL（一个 URL、一份凭据）：",
  "via banyan's own face:": "经 banyan 自身入口：",
  "no history at this path yet.": "该路径还没有历史。",
  "access rule": "访问规则",
  "allowed users: ": "允许的用户：",
  "(nobody)": "（无人）",
  "remove rule": "移除规则",
  "no rule — inherited from the repo policy.": "无规则——继承仓库策略。",
  "allowed users (space or comma separated; empty = nobody)": "允许的用户（空格或逗号分隔；留空 = 无人）",
  "update rule": "更新规则",
  "restrict to users": "限定到这些用户",

  // SettingsPage.tsx
  "{0} across {1} pack": "共 {0}，{1} 个包",
  "{0} across {1} packs": "共 {0}，{1} 个包",
  Size: "大小",
  Description: "描述",
  "the one-liner the repository list shows (truncated to the column's width).":
    "仓库列表展示的一行简介（按列宽截断）。",
  "what this repository is": "这个仓库是做什么的",
  save: "保存",
  "Force-Push Switch": "强推开关",
  "the subtree rollback guard is default-on; enabling rewrites is an explicit administrative opt-in.":
    "子树回滚护栏默认开启；放开改写是管理员的一次显式授权。",
  "admin required.": "需要管理员。",
  "disable force-push": "禁用强推",
  "enable force-push": "启用强推",
  "currently ": "当前",
  enabled: "已启用",
  disabled: "已禁用",
  "Branch Protection": "分支保护",
  "Rules": "规则",
  "{0} keeps the direct-push semantics (owner rank); {1} accepts no direct push at all — merge requests with the approval count are the only way in.":
    "{0} 保持直推语义（owner 级）；{1} 完全不接受直推——达到审批数的合并请求是唯一入口。",
  level: "级别",
  clear: "清除",
  "branch…": "分支…",
  "no rules — every branch takes the policy default.": "暂无规则——所有分支走策略默认值。",
  "required approvals": "所需审批数",
  "set rule": "设置规则",
  "an organizational label only — it folders the repository list and nothing else; the name and every URL stay untouched.":
    "仅是组织标签——只为仓库列表分拣目录，别无其他；名称与所有 URL 一概不动。",
  "— no group —": "— 无分组 —",
  move: "移动",
  Group: "分组",
  Rename: "重命名",
  rename: "重命名",
  "hashed addressing flips the registry alone; named addressing moves storage without touching object ids.":
    "哈希寻址只翻注册表；命名寻址搬存储而对象 id 不动。",
  "new name": "新名称",
  "rename {0} → {1}?": "重命名 {0} → {1}？",
  "Repack": "重打包",
  "merges pack fragments and delta-encodes the result (docs/storage/storage.md §5).":
    "合并 pack 碎片并对结果做增量编码（docs/storage/storage.md §5）。",
  "repack now": "立即重打包",
  "repacking…": "正在重打包…",
  "repacked: {0} → {1} packs, {2} objects, {3} → {4}{5}": "重打包完成：{0} → {1} 个 pack，{2} 个对象，{3} → {4}{5}",

  // members (GroupDetailPage + MembersPage, docs/design/data-model.md §16)
  rank: "权限",
  viewer: "只读",
  developer: "开发者",
  owner: "所有者",
  "inherited from {0}": "继承自 {0}",
  direct: "直接",
  "remove member {0}?": "移除成员 {0}？",
  remove: "移除",
  "username…": "用户名…",
  "add member": "添加成员",
  "group not found.": "找不到该分组。",
  "the group's member table — every labeled repository inherits these grants (the effective rank is the higher of the two).":
    "分组成员表——组内所有仓库继承这些授权（有效权限取两者中较高者）。",
  "no members yet — the group's repositories are open to every authenticated user until you add some.":
    "还没有成员——在添加成员之前，组内仓库对所有登录用户开放。",
  "added by": "添加人",
  added: "添加时间",
  "no repositories in this group yet.": "该分组还没有仓库。",
  "direct members plus the grants inherited from the group (marked) — the effective rank is the higher of the two; a direct grant must rank strictly above the inherited one.":
    "直接成员加上自分组继承的授权（带标记）——有效权限取两者中较高者；直接授权必须严格高于继承权限。",
  "no members — every authenticated user has this repository's baseline access.":
    "还没有成员——所有登录用户保有该仓库的基线访问。",

  // TokensSection.tsx
  "scopes are ceilings, never bypasses: a write-scoped token can read, an admin-scoped token can mint — none of them escapes the policies above.":
    "权限范围是上限而非旁路：write 令牌可读，admin 令牌可签发——但谁都逃不出上述策略。",
  "token name": "令牌名称",
  days: "有效天数",
  mint: "签发",
  "copy it now — the plaintext is shown exactly once:": "立即复制——明文只显示这一次：",
  copy: "复制",
  "none yet.": "暂无。",
  scope: "权限",
  expires: "过期时间",
  "(unnamed)": "（未命名）",
  never: "永不过期",
  revoked: "已吊销",
  "revoke {0} ({1})?": "吊销 {0}（{1}）？",
  revoke: "吊销",
  "Your Tokens · {0}": "我的令牌 · {0}",
  "All Tokens (Admin)": "全部令牌（管理员）",

  // FilePanel.tsx / MrFileEdit.tsx
  "plain view": "纯文本视图",
  "editor view": "编辑器视图",
  edit: "编辑",
  "commit message (default: edit {0})": "提交信息（默认：edit {0}）",
  "commit message (default: {0})": "提交信息（默认：{0}）",
  "commit to {0}": "提交到 {0}",
  cancel: "取消",
  "commits through the server-side assembly (the thin CLI's publish path) — branch protections apply.":
    "经由服务端组装提交（瘦 CLI 的发布路径）——分支保护照常生效。",
  "{0} is not a file on {1}.": "{0} 不是 {1} 上的文件。",
  "{0} is not editable in place (binary or over {1} KiB) — edit via clone.":
    "{0} 无法就地编辑（二进制或超过 {1} KiB）——请 clone 后修改。",
  "commits to {0}": "将提交到 {0}",
  "the edit rides the server-side assembly (the thin CLI's publish path) — branch protections apply, and the diff and mergeability recompute the moment it lands.":
    "编辑经由服务端组装提交（瘦 CLI 的发布路径）——分支保护照常生效，落地后 diff 与可合并性即时重算。",

  // ProfilePage.tsx
  "Appearance": "外观",
  "Password": "密码",
  "changing the password ends this session — log in again with the new one.":
    "修改密码将结束本次会话——请用新密码重新登录。",
  "password changed — log in again.": "密码已修改——请重新登录。",
  "your password was reset by an administrator — choose a new one to continue.": "你的密码已被管理员重置——请先设置新密码再继续。",
  "current password": "当前密码",
  "new password (8+ characters)": "新密码（至少 8 位）",
  "change password": "修改密码",
  auto: "自动",
  day: "白天",
  night: "黑夜",
  "day or night, whatever the OS says": "白天或黑夜，听操作系统的",
  "the monochrome: black primary on white": "单色风：白底黑主色",
  "warm-dark surfaces, the yellow primary": "暖黑底面，黄色主色",
  "following the system — currently {0}": "跟随系统——当前{0}",
  "Language": "语言",
  中文: "中文",
  English: "English",
  "following the browser — currently {0}": "跟随浏览器——当前 {0}",

  // SSHKeysPage.tsx
  "keys here authenticate the git-over-SSH transport — clone as ssh://git@<host>/<repo>.git; a key only ever names its owner.":
    "这里的密钥用于 git-over-SSH 传输认证——以 ssh://git@<host>/<repo>.git 克隆；一把密钥只代表其所有者。",
  "ssh-ed25519 AAAA… comment": "ssh-ed25519 AAAA… 备注",
  "add key": "添加密钥",
  "no keys yet.": "还没有密钥。",
  "Fingerprint": "指纹",
  "Type": "类型",
  "Comment": "备注",
  "Added": "添加时间",
  "delete key {0}?": "删除密钥 {0}？",
};
