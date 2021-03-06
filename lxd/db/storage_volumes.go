// +build linux,cgo,!agent

package db

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/lxc/lxd/lxd/db/query"
	"github.com/pkg/errors"
)

// StorageVolumeArgs is a value object holding all db-related details about a
// storage volume.
type StorageVolumeArgs struct {
	Name string

	// At least one of Type or TypeName must be set.
	Type     int
	TypeName string

	// At least one of PoolID or PoolName must be set.
	PoolID   int64
	PoolName string

	Snapshot bool

	Config       map[string]string
	Description  string
	CreationDate time.Time
}

// StorageVolumeNodeAddresses returns the addresses of all nodes on which the
// volume with the given name if defined.
//
// The volume name can be either a regular name or a volume snapshot name.
//
// The empty string is used in place of the address of the current node.
func (c *ClusterTx) StorageVolumeNodeAddresses(poolID int64, project, name string, typ int) ([]string, error) {
	nodes := []struct {
		id      int64
		address string
	}{}
	dest := func(i int) []interface{} {
		nodes = append(nodes, struct {
			id      int64
			address string
		}{})
		return []interface{}{&nodes[i].id, &nodes[i].address}

	}
	sql := `
SELECT nodes.id, nodes.address
  FROM nodes
  JOIN storage_volumes_all ON storage_volumes_all.node_id=nodes.id
  JOIN projects ON projects.id = storage_volumes_all.project_id
 WHERE storage_volumes_all.storage_pool_id=?
   AND projects.name=?
   AND storage_volumes_all.name=?
   AND storage_volumes_all.type=?
`
	stmt, err := c.tx.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	err = query.SelectObjects(stmt, dest, poolID, project, name, typ)
	if err != nil {
		return nil, err
	}

	addresses := []string{}
	for _, node := range nodes {
		address := node.address
		if node.id == c.nodeID {
			address = ""
		}
		addresses = append(addresses, address)
	}

	sort.Strings(addresses)

	if len(addresses) == 0 {
		return nil, ErrNoSuchObject
	}

	return addresses, nil
}

// Return the name of the node a storage volume is on.
func (c *Cluster) storageVolumeNodeGet(volumeID int64) (string, error) {
	name := ""
	query := `
SELECT nodes.name FROM storage_volumes_all
  JOIN nodes ON nodes.id=storage_volumes_all.node_id
   WHERE storage_volumes_all.id=?
`
	inargs := []interface{}{volumeID}
	outargs := []interface{}{&name}

	err := dbQueryRowScan(c.db, query, inargs, outargs)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNoSuchObject
		}

		return "", err
	}

	return name, nil
}

// Get the config of a storage volume.
func (c *Cluster) storageVolumeConfigGet(volumeID int64, isSnapshot bool) (map[string]string, error) {
	var key, value string
	var query string
	if isSnapshot {
		query = "SELECT key, value FROM storage_volumes_snapshots_config WHERE storage_volume_snapshot_id=?"
	} else {
		query = "SELECT key, value FROM storage_volumes_config WHERE storage_volume_id=?"
	}
	inargs := []interface{}{volumeID}
	outargs := []interface{}{key, value}

	results, err := queryScan(c.db, query, inargs, outargs)
	if err != nil {
		return nil, err
	}

	config := map[string]string{}

	for _, r := range results {
		key = r[0].(string)
		value = r[1].(string)

		config[key] = value
	}

	return config, nil
}

// StorageVolumeDescriptionGet gets the description of a storage volume.
func (c *Cluster) StorageVolumeDescriptionGet(volumeID int64) (string, error) {
	description := sql.NullString{}
	query := "SELECT description FROM storage_volumes_all WHERE id=?"
	inargs := []interface{}{volumeID}
	outargs := []interface{}{&description}

	err := dbQueryRowScan(c.db, query, inargs, outargs)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNoSuchObject
		}
		return "", err
	}

	return description.String, nil
}

// StorageVolumeNextSnapshot returns the index of the next snapshot of the storage
// volume with the given name should have.
//
// Note, the code below doesn't deal with snapshots of snapshots.
// To do that, we'll need to weed out based on # slashes in names
func (c *Cluster) StorageVolumeNextSnapshot(name string, typ int) int {
	base := "snap"
	length := len(base)
	q := fmt.Sprintf(`
SELECT storage_volumes_snapshots.name FROM storage_volumes_snapshots
  JOIN storage_volumes ON storage_volumes_snapshots.storage_volume_id=storage_volumes.id
 WHERE storage_volumes.type=?
   AND storage_volumes.name=?
   AND SUBSTR(storage_volumes_snapshots.name,1,?)=?
`)
	var numstr string
	inargs := []interface{}{typ, name, length, base}
	outfmt := []interface{}{numstr}
	results, err := queryScan(c.db, q, inargs, outfmt)
	if err != nil {
		return 0
	}
	max := 0

	for _, r := range results {
		substr := r[0].(string)
		var num int
		count, err := fmt.Sscanf(substr, base+"%d", &num)
		if err != nil || count != 1 {
			continue
		}
		if num >= max {
			max = num + 1
		}
	}

	return max
}

// StorageVolumeIsAvailable checks that if a custom volume available for being attached.
//
// Always return true for non-Ceph volumes.
//
// For Ceph volumes, return true if the volume is either not attached to any
// other container, or attached to containers on this node.
func (c *Cluster) StorageVolumeIsAvailable(pool, volume string) (bool, error) {
	isAvailable := false

	err := c.Transaction(func(tx *ClusterTx) error {
		id, err := tx.StoragePoolID(pool)
		if err != nil {
			return errors.Wrapf(err, "Fetch storage pool ID for %q", pool)
		}

		driver, err := tx.StoragePoolDriver(id)
		if err != nil {
			return errors.Wrapf(err, "Fetch storage pool driver for %q", pool)
		}

		if driver != "ceph" {
			isAvailable = true
			return nil
		}

		node, err := tx.NodeName()
		if err != nil {
			return errors.Wrapf(err, "Fetch node name")
		}

		containers, err := tx.instanceListExpanded()
		if err != nil {
			return errors.Wrapf(err, "Fetch instances")
		}

		for _, container := range containers {
			for _, device := range container.Devices {
				if device["type"] != "disk" {
					continue
				}
				if device["pool"] != pool {
					continue
				}
				if device["source"] != volume {
					continue
				}
				if container.Node != node {
					// This ceph volume is already attached
					// to a container on a different node.
					return nil
				}
			}
		}
		isAvailable = true

		return nil
	})
	if err != nil {
		return false, err
	}

	return isAvailable, nil
}

// Updates the description of a storage volume.
func storageVolumeDescriptionUpdate(tx *sql.Tx, volumeID int64, description string, isSnapshot bool) error {
	var table string
	if isSnapshot {
		table = "storage_volumes_snapshots"
	} else {
		table = "storage_volumes"
	}
	stmt := fmt.Sprintf("UPDATE %s SET description=? WHERE id=?", table)
	_, err := tx.Exec(stmt, description, volumeID)
	return err
}

// Add a new storage volume config into database.
func storageVolumeConfigAdd(tx *sql.Tx, volumeID int64, volumeConfig map[string]string, isSnapshot bool) error {
	var str string
	if isSnapshot {
		str = "INSERT INTO storage_volumes_snapshots_config (storage_volume_snapshot_id, key, value) VALUES(?, ?, ?)"
	} else {
		str = "INSERT INTO storage_volumes_config (storage_volume_id, key, value) VALUES(?, ?, ?)"
	}
	stmt, err := tx.Prepare(str)
	defer stmt.Close()
	if err != nil {
		return err
	}

	for k, v := range volumeConfig {
		if v == "" {
			continue
		}

		_, err = stmt.Exec(volumeID, k, v)
		if err != nil {
			return err
		}
	}

	return nil
}

// Delete storage volume config.
func storageVolumeConfigClear(tx *sql.Tx, volumeID int64, isSnapshot bool) error {
	var stmt string
	if isSnapshot {
		stmt = "DELETE FROM storage_volumes_snapshots_config WHERE storage_volume_snapshot_id=?"
	} else {
		stmt = "DELETE FROM storage_volumes_config WHERE storage_volume_id=?"
	}
	_, err := tx.Exec(stmt, volumeID)
	if err != nil {
		return err
	}

	return nil
}

// Get the IDs of all volumes with the given name and type associated with the
// given pool, regardless of their node_id column.
func storageVolumeIDsGet(tx *sql.Tx, project, volumeName string, volumeType int, poolID int64) ([]int64, error) {
	ids, err := query.SelectIntegers(tx, `
SELECT storage_volumes_all.id
  FROM storage_volumes_all
  JOIN projects ON projects.id = storage_volumes_all.project_id
 WHERE projects.name=?
   AND storage_volumes_all.name=?
   AND storage_volumes_all.type=?
   AND storage_volumes_all.storage_pool_id=?
`, project, volumeName, volumeType, poolID)
	if err != nil {
		return nil, err
	}
	ids64 := make([]int64, len(ids))
	for i, id := range ids {
		ids64[i] = int64(id)
	}
	return ids64, nil
}

// StorageVolumeCleanupImages removes the volumes with the given fingerprints.
func (c *Cluster) StorageVolumeCleanupImages(fingerprints []string) error {
	stmt := fmt.Sprintf(
		"DELETE FROM storage_volumes WHERE type=? AND name NOT IN %s",
		query.Params(len(fingerprints)))
	args := []interface{}{StoragePoolVolumeTypeImage}
	for _, fingerprint := range fingerprints {
		args = append(args, fingerprint)
	}
	err := exec(c.db, stmt, args...)
	return err
}

// StorageVolumeMoveToLVMThinPoolNameKey upgrades the config keys of LVM
// volumes.
func (c *Cluster) StorageVolumeMoveToLVMThinPoolNameKey() error {
	err := exec(c.db, "UPDATE storage_pools_config SET key='lvm.thinpool_name' WHERE key='volume.lvm.thinpool_name';")
	if err != nil {
		return err
	}

	err = exec(c.db, "DELETE FROM storage_volumes_config WHERE key='lvm.thinpool_name';")
	if err != nil {
		return err
	}
	err = exec(c.db, "DELETE FROM storage_volumes_snapshots_config WHERE key='lvm.thinpool_name';")
	if err != nil {
		return err
	}

	return nil
}
