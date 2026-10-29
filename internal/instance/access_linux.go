package instance

import (
	"context"
	"database/sql"
	"errors"
)

type Role string

const (
	Reader     Role = "reader"
	Writer     Role = "writer"
	Maintainer Role = "maintainer"
)

func (r Role) Allows(required Role) bool {
	switch required {
	case Reader:
		return r == Reader || r == Writer || r == Maintainer
	case Writer:
		return r == Writer || r == Maintainer
	case Maintainer:
		return r == Maintainer
	}
	return false
}

// Both individual lookup and listing derive authority from the same ownership
// and membership rules. Tokens cap that authority and never inherit site admin.
const projectAccess = `SELECT p.id,u.name,p.name,p.state,
 CASE WHEN p.owner=? OR ? THEN 'maintainer' ELSE COALESCE(m.role,'') END
 FROM projects p JOIN users u ON p.owner=u.id
 LEFT JOIN members m ON m.project=p.id AND m.user_id=?`

func (p *Project) limit(actor Principal) {
	if p.Role.Allows(Reader) && actor.Token && !actor.Role.Allows(p.Role) {
		p.Role = actor.Role
	}
}

type Member struct {
	User  User
	Role  Role
	Owner bool
}

var ErrMember = errors.New("user not found or disabled")
var ErrOwner = errors.New("the project owner always has maintainer access")
var ErrRole = errors.New("choose reader, writer or maintainer access")

func (s *Store) Members(ctx context.Context, actor Principal, owner, name string) ([]Member, error) {
	p, err := s.Project(ctx, actor, owner, name, Maintainer)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id,u.name,u.admin,u.disabled,'maintainer' AS role,1 AS owner FROM users u WHERE u.name=?
 UNION ALL SELECT u.id,u.name,u.admin,u.disabled,m.role,0 FROM members m JOIN users u ON u.id=m.user_id WHERE m.project=?
 ORDER BY owner DESC,name`, p.Owner, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.User.ID, &m.User.Name, &m.User.Admin, &m.User.Disabled, &m.Role, &m.Owner); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// An empty role removes membership. Ownership is implicit and cannot be changed
// through collaborator controls. Already-authorized requests may finish.
func (s *Store) SetMember(ctx context.Context, actor Principal, owner, name, username string, role Role) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := projectFor(ctx, tx, actor, owner, name, Maintainer)
	if err != nil {
		return err
	}
	if role != "" && !role.Allows(Reader) {
		return ErrRole
	}
	if username == p.Owner {
		return ErrOwner
	}
	var user User
	err = tx.QueryRowContext(ctx, "SELECT id,name,disabled FROM users WHERE name=?", username).Scan(&user.ID, &user.Name, &user.Disabled)
	if errors.Is(err, sql.ErrNoRows) || user.Disabled && role != "" {
		return ErrMember
	}
	if err != nil {
		return err
	}
	if role == "" {
		_, err = tx.ExecContext(ctx, "DELETE FROM members WHERE project=? AND user_id=?", p.ID, user.ID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO members(project,user_id,role) VALUES(?,?,?) ON CONFLICT(project,user_id) DO UPDATE SET role=excluded.role`, p.ID, user.ID, role)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
