package qdb

import (
	"fmt"
	"errors"
	"encoding/binary"
	"github.com/piotrnar/gocoin/btc"
	"github.com/piotrnar/gocoin/qdb"
)

/*
Each unspent key is prevOutIdxLen bytes long - thats part of the tx hash xored witth vout
Eech value is variable length:
  [0:32] - TxPrevOut.Hash
  [32:36] - TxPrevOut.Vout LSB
  [36:44] - Value LSB
  [44:48] - BlockHeight LSB (where mined)
  [48:] - Pk_script (in DBfile first 4 bytes are LSB length)
*/


const (
	prevOutIdxLen = qdb.KeySize
)

var (
	KeepBlocksBack int // Zero means: keep all the records in memory
)


type unspentDb struct {
	dir string
	tdb [0x100] *qdb.DB
	defragIndex int
	defragCount uint64
	nosyncinprogress bool
	notifyTx btc.TxNotifyFunc
	lastHeight uint32
}

func newUnspentDB(dir string, lasth uint32) (db *unspentDb) {
	db = new(unspentDb)
	db.dir = dir
	db.lastHeight = lasth
	for i := range db.tdb {
		if ((i&3)==0) {
			fmt.Print("\rLoading unspent DB - ", 100*i/len(db.tdb), "% complete ... ")
		}
		db.dbN(i) // Load each of the sub-DBs into memory
	}
	fmt.Print("\r                                                              \r")
	return
}


func (db *unspentDb) dbN(i int) (*qdb.DB) {
	if db.tdb[i]==nil {
		if KeepBlocksBack != 0 {
			db.tdb[i], _ = qdb.NewDB(db.dir+fmt.Sprintf("%02x/", i), &qdb.DBConfig{KeepInMem:func (v []byte) bool {
				return KeepBlocksBack==0 ||
					int(binary.LittleEndian.Uint32(v[44:48])) > int(db.lastHeight) - KeepBlocksBack
			}})
		} else {
			db.tdb[i], _ = qdb.NewDB(db.dir+fmt.Sprintf("%02x/", i), nil)
		}
		if db.nosyncinprogress {
			db.tdb[i].NoSync()
		}
	}
	return db.tdb[i]
}


func getUnspIndex(po *btc.TxPrevOut) (qdb.KeyType) {
	return qdb.KeyType(binary.LittleEndian.Uint64(po.Hash[:8]) ^ uint64(po.Vout))
}


func (db *unspentDb) get(po *btc.TxPrevOut) (res *btc.TxOut, e error) {
	ind := getUnspIndex(po)
	val := db.dbN(int(po.Hash[31])).Get(ind)
	if val==nil {
		e = errors.New("Unspent not found")
		return
	}

	if len(val)<48 {
		panic(fmt.Sprint("unspent record too short:", len(val)))
	}

	res = new(btc.TxOut)
	res.Value = binary.LittleEndian.Uint64(val[36:44])
	res.BlockHeight = binary.LittleEndian.Uint32(val[44:48])
	res.Pk_script = make([]byte, len(val)-48)
	copy(res.Pk_script, val[48:])
	return
}


func (db *unspentDb) add(idx *btc.TxPrevOut, Val_Pk *btc.TxOut) {
	if db.notifyTx!=nil {
		db.notifyTx(idx, Val_Pk)
	}
	v := make([]byte, 48+len(Val_Pk.Pk_script))
	copy(v[0:32], idx.Hash[:])
	binary.LittleEndian.PutUint32(v[32:36], idx.Vout)
	binary.LittleEndian.PutUint64(v[36:44], Val_Pk.Value)
	binary.LittleEndian.PutUint32(v[44:48], Val_Pk.BlockHeight)
	copy(v[48:], Val_Pk.Pk_script)
	ind := getUnspIndex(idx)
	db.dbN(int(idx.Hash[31])).Put(ind, v)
}


func (db *unspentDb) del(idx *btc.TxPrevOut) {
	if db.notifyTx!=nil {
		db.notifyTx(idx, nil)
	}
	db.dbN(int(idx.Hash[31])).Del(getUnspIndex(idx))
}


func bin2unspent(v []byte, ad *btc.BtcAddr) (nr btc.OneUnspentTx) {
	copy(nr.TxPrevOut.Hash[:], v[0:32])
	nr.TxPrevOut.Vout = binary.LittleEndian.Uint32(v[32:36])
	nr.Value = binary.LittleEndian.Uint64(v[36:44])
	nr.MinedAt = binary.LittleEndian.Uint32(v[44:48])
	nr.BtcAddr = ad
	return
}


func (db *unspentDb) GetAllUnspent(addr []*btc.BtcAddr, quick bool) (res btc.AllUnspentTx) {
	if quick {
		addrs := make(map[uint64]*btc.BtcAddr, len(addr))
		for i := range addr {
			addrs[binary.LittleEndian.Uint64(addr[i].Hash160[0:8])] = addr[i]
		}
		for i := range db.tdb {
			db.dbN(i).Browse(func(k qdb.KeyType, v []byte) bool {
				scr := v[48:]
				if len(scr)==25 && scr[0]==0x76 && scr[1]==0xa9 && scr[2]==0x14 && scr[23]==0x88 && scr[24]==0xac {
					if ad, ok := addrs[binary.LittleEndian.Uint64(scr[3:3+8])]; ok {
						res = append(res, bin2unspent(v[:48], ad))
					}
				}
				return true
			})
		}
	} else {
		for i := range db.tdb {
			db.dbN(i).Browse(func(k qdb.KeyType, v []byte) bool {
				for a := range addr {
					if addr[a].Owns(v[48:]) {
						res = append(res, bin2unspent(v[:48], addr[a]))
					}
				}
				return true
			})
		}
	}
	return
}


func (db *unspentDb) commit(changes *btc.BlockChanges) {
	// Now ally the unspent changes
	for k, v := range changes.AddedTxs {
		db.add(&k, v)
	}
	for k, _ := range changes.DeledTxs {
		db.del(&k)
	}
}


func (db *unspentDb) stats() (s string) {
	var cnt, sum, cntlow, sumlow uint64
	for i := range db.tdb {
		db.dbN(i).Browse(func(k qdb.KeyType, v []byte) bool {
			value := binary.LittleEndian.Uint64(v[36:44])
			sum += value
			cnt++
			if value<1e5 {
				sumlow += value
				cntlow++
			}
			return true
		})
	}
	s = fmt.Sprintf("UNSPENT: %.8f BTC in %d outputs. Defrags:%d. BH:%d(%d)\n",
		float64(sum)/1e8, cnt, db.defragCount, db.lastHeight, KeepBlocksBack)
	s += fmt.Sprintf(" %d outputs below 0.001 BTC, with %.2f BTC total\n",
		cntlow, float64(sumlow)/1e8)
	return
}


func (db *unspentDb) sync() {
	db.nosyncinprogress = false
	for i := range db.tdb {
		if db.tdb[i]!=nil {
			db.tdb[i].Sync()
		}
	}
}

func (db *unspentDb) nosync() {
	db.nosyncinprogress = true
	for i := range db.tdb {
		if db.tdb[i]!=nil {
			db.tdb[i].NoSync()
		}
	}
}

func (db *unspentDb) save() {
	for i := range db.tdb {
		if db.tdb[i]!=nil {
			db.tdb[i].Defrag()
		}
	}
}

func (db *unspentDb) close() {
	for i := range db.tdb {
		if db.tdb[i]!=nil {
			db.tdb[i].Close()
			db.tdb[i] = nil
		}
	}
}

func (db *unspentDb) idle() bool {
	for _ = range db.tdb {
		db.defragIndex++
		if db.defragIndex >= len(db.tdb) {
			db.defragIndex = 0
		}
		if db.tdb[db.defragIndex]!=nil && db.tdb[db.defragIndex].Defrag() {
			db.defragCount++
			return true
		}
	}
	return false
}
