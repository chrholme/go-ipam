package ipam

import (
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"
)

type sql struct {
	db *sqlx.DB
}

type prefixJSON struct {
	Prefix
	AvailableChildPrefixes map[string]bool // available child prefixes of this prefix
	ChildPrefixLength      int             // the length of the child prefixes
	IPs                    map[string]bool // The ips contained in this prefix
	Version                int64           // Version is used for optimistic locking
}

func (p prefixJSON) toPrefix() Prefix {
	return Prefix{
		Cidr:                   p.Cidr,
		ParentCidr:             p.ParentCidr,
		availableChildPrefixes: p.AvailableChildPrefixes,
		childPrefixLength:      p.ChildPrefixLength,
		Ips:                    p.IPs,
		version:                p.Version,
	}
}

func (p Prefix) toPrefixJSON() prefixJSON {
	return prefixJSON{
		Prefix: Prefix{
			Cidr:       p.Cidr,
			ParentCidr: p.ParentCidr,
		},
		AvailableChildPrefixes: p.availableChildPrefixes,
		ChildPrefixLength:      p.childPrefixLength,
		IPs:                    p.Ips,
		Version:                p.version,
	}
}

func (s *sql) prefixExists(prefix Prefix, tenantid string) (*Prefix, bool) {
	p, err := s.ReadPrefix(prefix.Cidr, tenantid)
	if err != nil {
		return nil, false
	}
	return &p, true
}

func (s *sql) CreatePrefix(prefix Prefix, tenantid string) (Prefix, error) {
	existingPrefix, exists := s.prefixExists(prefix, tenantid)
	if exists {
		return *existingPrefix, nil
	}
	prefix.version = int64(0)
	pj, err := json.Marshal(prefix.toPrefixJSON())
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to marshal prefix:%v", err)
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to start transaction:%v", err)
	}
	tx.MustExec("INSERT INTO prefixes (cidr, prefix, tenantid) VALUES ($1, $2, $3)", prefix.Cidr, pj, tenantid)
	return prefix, tx.Commit()
}

func (s *sql) ReadPrefix(prefix string, tenantid string) (Prefix, error) {
	var result []byte
	err := s.db.Get(&result, "SELECT prefix FROM prefixes WHERE cidr=$1 AND tenantid=$2", prefix, tenantid)
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to read prefix:%v", err)
	}
	var pre prefixJSON
	err = json.Unmarshal(result, &pre)
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to unmarshal prefix:%v", err)
	}

	return pre.toPrefix(), nil
}

func (s *sql) ReadAllPrefixes(tenantid string) ([]Prefix, error) {
	var prefixes [][]byte
	err := s.db.Select(&prefixes, "SELECT prefix FROM prefixes WHERE tenantid=$1", tenantid)
	if err != nil {
		return nil, fmt.Errorf("unable to read prefixes:%v", err)
	}

	result := []Prefix{}
	for _, v := range prefixes {
		var pre prefixJSON
		err = json.Unmarshal(v, &pre)
		if err != nil {
			return nil, fmt.Errorf("unable to unmarshal prefix:%v", err)
		}
		result = append(result, pre.toPrefix())
	}
	return result, nil
}

// UpdatePrefix tries to update the prefix.
// Returns OptimisticLockError if it does not succeed due to a concurrent update.
func (s *sql) UpdatePrefix(prefix Prefix, tenantid string) (Prefix, error) {
	oldVersion := prefix.version
	prefix.version = oldVersion + 1
	pn, err := json.Marshal(prefix.toPrefixJSON())
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to marshal prefix:%v", err)
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to start transaction:%v", err)
	}
	result := tx.MustExec("SELECT prefix FROM prefixes WHERE cidr=$1 AND tenantid=$2 AND prefix->>'Version'=$3 FOR UPDATE", prefix.Cidr, tenantid, oldVersion)
	rows, err := result.RowsAffected()
	if err != nil {
		return Prefix{}, err
	}
	if rows == 0 {
		err := tx.Rollback()
		if err != nil {
			return Prefix{}, newOptimisticLockError("select for update did not effect any row, but rollback did not work:" + err.Error())
		}
		return Prefix{}, newOptimisticLockError("select for update did not effect any row")
	}
	result = tx.MustExec("UPDATE prefixes SET prefix=$1 WHERE cidr=$2 AND tenantid=$3 AND prefix->>'Version'=$4", pn, prefix.Cidr, tenantid, oldVersion)
	rows, err = result.RowsAffected()
	if err != nil {
		return Prefix{}, err
	}
	if rows == 0 {
		err := tx.Rollback()
		if err != nil {
			return Prefix{}, newOptimisticLockError("updatePrefix did not effect any row, but rollback did not work:" + err.Error())
		}
		return Prefix{}, newOptimisticLockError("updatePrefix did not effect any row")
	}
	return prefix, tx.Commit()
}

func (s *sql) DeletePrefix(prefix Prefix, tenantid string) (Prefix, error) {
	tx, err := s.db.Beginx()
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to start transaction:%v", err)
	}
	tx.MustExec("DELETE from prefixes WHERE cidr=$1 AND tenantid=$2", prefix.Cidr, tenantid)
	return prefix, tx.Commit()
}
