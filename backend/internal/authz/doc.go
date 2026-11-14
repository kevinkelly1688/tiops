// @author: kevinkelly
// @license: Apache License 2.0
// @file: doc.go
// @time: 2026/10/28

// Package authz 实现 A3 §2、§6 的身份、RBAC、审批策略、口令、TOTP 与会话。仅依赖标准库。
//
// # API 摘要
//
// 角色与权限（rbac.go）
//
//	type Role string                      // RoleViewer/RoleDeveloper/RoleSRE/RoleDBA/RoleApprover/RoleAdmin/RoleAuditor
//	type Cap string                       // CapViewOverview … CapShellApprove，共 20 项
//	type Grant int                        // GrantNone / GrantAllow(✔) / GrantApproval(审) / GrantScoped(范)
//	func Matrix() map[Role]map[Cap]Grant  // A3 §2.2 矩阵的副本
//	func RoleGrant(r Role, c Cap) Grant
//	func AllRoles() []Role; func AllCaps() []Cap; func (Role) Valid() bool; func (Grant) Can() bool
//
//	type User struct{ Name, Display string; Roles map[string][]Role; DBs []string; Disabled, HasTOTP bool; Created time.Time }
//	func (u User) RolesFor(env string) []Role        // env 键存在则覆盖 "*"
//	func (u User) Grant(env string, c Cap) Grant     // 多角色取最强；被禁用恒为 GrantNone
//	func (u User) Can(env string, c Cap) bool
//	func (u User) HasRole(env string, r Role) bool   // 被禁用恒为 false
//	func (u User) CanViewSQLOf(env, db string) bool  // developer 仅限 DBs
//
// 审批（A3 §2.4）
//
//	func ApprovalPolicy(risk, env string, emergency bool) (levels int, applicantMFA bool)
//	func DefaultPolicy() PolicyTable                  // 可配置策略表（policy.go）：Validate 带硬性下限，Lookup 固定紧急变更规则
//	func CanApprove(u User, env, risk string) bool
//	func ValidateApproval(applicant string, prior []string, approver User, env, risk string) error
//	// 错误：ErrSelfApproval / ErrDuplicateApproval / ErrNotApprover（用 errors.Is 判断）
//
// 口令（password.go）
//
//	func HashPassword(pw string) (string, error)     // pbkdf2-sha256$iter$salt$hash，iter=DefaultIterations(210000)
//	func VerifyPassword(pw, encoded string) bool     // 恒定时间
//	func CheckPassword(name, pw string) error        // ≥10 位、≥3 类字符、不含用户名（≤128 位）
//
// TOTP（totp.go，RFC 6238：HMAC-SHA1 / 30 秒 / 6 位 / ±1 窗口）
//
//	func NewTOTPSecret() string
//	func TOTPCode(secret string, t time.Time) string
//	func ProvisioningURI(issuer, user, secret string) string
//
// 用户库（store.go）
//
//	func OpenStore(path string, masterKey []byte) (*Store, error)
//	func (s *Store) Add(name, display, password string, roles map[string][]Role, dbs []string) error
//	func (s *Store) SetPassword(name, pw string) error
//	func (s *Store) SetRoles(name string, roles map[string][]Role) error
//	func (s *Store) SetDBs(name string, dbs []string) error
//	func (s *Store) SetDisabled(name string, disabled bool) error
//	func (s *Store) Delete(name string) error
//	func (s *Store) List() []User
//	func (s *Store) Get(name string) (User, bool)
//	func (s *Store) Authenticate(name, password, otp, ip string, now time.Time) (User, error)
//	func (s *Store) BeginTOTP(name string) (secret, uri string, err error)
//	func (s *Store) ConfirmTOTP(name, code string, now time.Time) error
//	func (s *Store) VerifyOTP(name, code string, now time.Time) bool
//	func (s *Store) ResetTOTP(name string) error
//	// 错误：ErrInvalidCredentials（统一文案“用户名或口令错误”）、ErrLocked（*LockedError 带 Until）、
//	// ErrNotFound、ErrExists、ErrLastAdmin、ErrInvalidName、ErrTOTPBound、ErrTOTPNotPending、ErrBadOTP
//
// 会话（session.go）
//
//	func NewSessions(idle, abs time.Duration) *Sessions       // 默认 30 分钟 / 8 小时
//	func (s *Sessions) Create(user string) (token, csrf string)
//	func (s *Sessions) Get(token string, now time.Time) (Session, bool)
//	func (s *Sessions) Destroy(token string)
//	func (s *Sessions) RevokeUser(name string) int
//	func (s *Sessions) CheckCSRF(token, csrf string) bool
//	func (s *Sessions) Rotate(old string, now time.Time) (token, csrf string, ok bool) // 登录后轮换会话 ID
//	func (s *Sessions) Sweep(now time.Time) int; func (s *Sessions) Len() int
//
// 自定义角色与能力目录（roles.go，约束见该文件与 docs/用户手册.md 5.3）
//
//	func OpenRoleStore(path string) (*RoleStore, error)   // roles.json；打开即发布到包级注册表（Role.Valid / User.Grant 等据此识别）
//	func (s *RoleStore) Create(actor User, env string, in RoleInput, now time.Time) (CustomRole, error)
//	func (s *RoleStore) Update(actor User, env, id string, up RoleUpdate, now time.Time) (before, after CustomRole, err error)
//	func (s *RoleStore) Delete(id string, users *Store) (CustomRole, error)   // 仍有成员持有 → in_use
//	func Catalog() []CapGroup; func AllowedGrants(Cap) []Grant; func IsLockedCap(Cap) bool
//	func ValidateCaps(map[Cap]Grant) error; func CheckGrantable(actor User, env string, prev, next map[Cap]Grant) error
//	func KnownRoles() []Role; func RoleDisplayName(Role) (name string, builtin, ok bool); func RoleCaps(Role) (map[Cap]Grant, bool)
//	// 错误：*RoleError（Code：invalid / builtin / locked_cap / escalation / exists / limit / in_use / not_found，errors.Is(err, ErrRoleXxx)）
//	// AllRoles() 仍只返回内置 7 个；含自定义角色用 KnownRoles()。注册表是进程级单例，测试里用 ClearCustomRoles() 清理。
//
// 用户资料（profile.go）
//
//	type Profile struct{ Display, Dept, Email, Phone, Note string }; func (Profile) Normalize() (Profile, error)  // ErrInvalidProfile
//	func (s *Store) AddWithProfile(name, password string, roles map[string][]Role, dbs []string, p Profile) error
//	func (s *Store) UpdateProfile(name string, up ProfileUpdate) (changed []string, u User, err error)
//	func (s *Store) RecordLogin(name string, now time.Time) error      // 只在登录成功时调用；30 秒内重复只更新内存
//	func (s *Store) LockedUntil(name string, now time.Time) (time.Time, bool)
//	func (s *Store) Unlock(name string, now time.Time) (wasLocked bool, until time.Time, err error) // 只清用户名维度（登录 / 动态码），不清 IP 维度
//
// 演示账号（seed.go）
//
//	const DemoPassword = "Tidbops-Demo-1"
//	func SeedDemoUsers(s *Store) error // 仅在库为空时创建 9 个演示账号；调用方只应在回放 / 演示模式调用
//
// # 设计要点与注意事项
//
//   - 用户名在库内规整为小写（Add / Get / Authenticate 均不区分大小写），格式 ^[a-z0-9][a-z0-9._@-]{1,63}$。
//   - 登录失败按“用户名”与“IP”两个维度各自计数：连续 5 次失败（10 分钟窗口内）锁定 10 分钟；
//     不存在的用户名同样计数与锁定；成功登录只清零用户维度计数。计数仅在内存，重启即清零。
//   - 已绑定 TOTP 的用户缺少或错误的 otp 与口令错误返回同一个 ErrInvalidCredentials（并计入失败），
//     因此登录页应始终提供动态码输入框，不能依赖“先输口令、再提示输入动态码”的两步流程。
//   - TOTP 防重放：每个用户记录最近一次被接受的时间步长（持久化），只接受更新的步长；
//     VerifyOTP / ConfirmTOTP 的失败同样计数，5 次后锁定 10 分钟（独立于登录锁定）。
//   - 禁用 / 删除用户、改口令、重置 TOTP 后，调用方应调用 Sessions.RevokeUser 吊销其会话（两个类型互不依赖）。
//   - “最后一个管理员”指仍启用且在任一环境拥有 admin 角色的用户；删除 / 禁用 / 降级会使其归零时返回 ErrLastAdmin。
//   - 职责分离：合并角色（如 dba+approver）时，同一变更的自批由 ValidateApproval 拦截；调用方还需保证
//     审批人与执行人的分离（A3 §2.1），本包不持有变更状态。
//   - 紧急变更：test 无紧急通道，按常规策略；staging / prod 至少 2 级；prod 的“事后复盘必填”由调用方处理。
//     未知的 risk / env 按最严格（2 级 + MFA）处理。
//   - Store 的所有修改先作用于副本、落盘（临时文件 + fsync + rename）成功后才替换内存状态。
package authz
