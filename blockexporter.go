package btcexport

import (
	"compress/gzip"
	"encoding/csv"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/database"
	"github.com/btcsuite/btcutil"
)

var (
	// block91842TxHash is one of the two transactions which violate the rules
	// set forth in BIP0030.  It is defined as a package level variable to avoid
	// the need to create a new instance every time a check is needed.
	block91842TxHash = newHashFromStr("d5d27987d2a3dfc724e359870c6644b40e497bdc0589a033220fe15429d88599")

	// block91880TxHash is one of the two transactions which violate the rules
	// set forth in BIP0030.  It is defined as a package level variable to avoid
	// the need to create a new instance every time a check is needed.
	block91880TxHash = newHashFromStr("e3bf3d07d4b0375638d5f1db5255fe07ba2c4cb067cd81b84ee974b6585fb468")
)

// recordWriter writes records (vectors of strings) to a backing writer. The
// backing writer is capped at an approximate size limit and once that size
// limit is exceeded, the backing writer is closed and a new one is opened. The
// data written is compressed using gzip.
type recordWriter struct {
	capacity       int
	rotatingWriter *RotatingWriter
	gzipWriter     *gzip.Writer
	csvWriter      *csv.Writer
}

// newRecordWriter constructs a new recordWriter that writes to the writer
// parameter and maintains a byte capacity per backing writer.
func newRecordWriter(writer *RotatingWriter, capacity int) *recordWriter {
	rw := recordWriter{
		capacity:       capacity,
		rotatingWriter: writer,
	}
	rw.gzipWriter = gzip.NewWriter(rw.rotatingWriter)
	rw.csvWriter = csv.NewWriter(rw.gzipWriter)
	return &rw
}

// Write encodes a record (vector of strings) as CSV and writes it to the
// backing writer. If the current backing writer has more data than the
// recordWriter's capacity, a new one will be opened.
func (rw *recordWriter) Write(record []string) error {
	if rw.rotatingWriter.BytesWritten() > rw.capacity {
		rw.csvWriter.Flush()
		if err := rw.csvWriter.Error(); err != nil {
			return err
		}

		err := rw.gzipWriter.Close()
		if err != nil {
			return err
		}

		err = rw.rotatingWriter.RotateWriter()
		if err != nil {
			return err
		}

		rw.gzipWriter.Reset(rw.rotatingWriter)
	}

	return rw.csvWriter.Write(record)
}

// Close flushes any unwritten data to the backing writer and closes all
// writers.
func (rw *recordWriter) Close() error {
	rw.csvWriter.Flush()
	if err := rw.csvWriter.Error(); err != nil {
		return err
	}

	err := rw.gzipWriter.Close()
	if err != nil {
		return err
	}

	err = rw.rotatingWriter.Close()
	if err != nil {
		return err
	}

	return nil
}

type blockDescriptor struct {
	hash   *chainhash.Hash
	height int32
}

// Config is used to construct a new BlockExporter.
type Config struct {
	DB        database.DB
	Chain     *blockchain.BlockChain
	NetParams *chaincfg.Params

	// OpenWriter creates a new rotating writer to the output destination.
	OpenWriter WriterFactory

	// StartHeight is the block height to export from.
	StartHeight uint

	// EndHeight is the block height to export until. If this field is 0, it
	// will default to the height of the last confirmed block on current chain.
	EndHeight uint

	// ConfirmedDepth is the depth required for a block to be considered
	// confirmed. A confirmed block should never be reorganized out of the
	// chain. The confirmed depth is used to determine the ending height of the
	// export if no EndHeight is set explicitly.
	ConfirmedDepth uint

	// FileSizeLimit is the maximum size in bytes that an output file is allowed
	// to be. If the record data exceeds the file size limit, it will be split
	// across multiple files.
	FileSizeLimit int
}

// BlockExporter is used to read a range of raw Bitcoin blocks from the
// database, encode them as tabular data in a text format (ie. CSV), and write
// them to an output data store. Data is not guaranteed to be written to the
// output in any particular order as blocks are processed in parallel and data
// is split across multiple files with a maximum size limit.
type BlockExporter struct {
	db      database.DB
	chain   *blockchain.BlockChain
	encoder BlockEncoder
	cfg     *Config

	numProcessed uint32
	errChan      chan error
	doneChan     chan struct{}
	quit         chan struct{}
}

// New constructs and returns a new BlockExporter.
func New(cfg Config) (*BlockExporter, error) {
	if cfg.EndHeight == 0 {
		// Export until the last confirmed block. The confirmed depth is chosen
		// such that chain reorgs longer than the depth are highly unlikely.
		snapshot := cfg.Chain.BestSnapshot()
		cfg.EndHeight = uint(snapshot.Height) - cfg.ConfirmedDepth
	}

	exporter := BlockExporter{
		db:       cfg.DB,
		chain:    cfg.Chain,
		encoder:  &blockEncoder{cfg.NetParams},
		cfg:      &cfg,
		errChan:  make(chan error),
		doneChan: make(chan struct{}),
		quit:     make(chan struct{}),
	}
	return &exporter, nil
}

// Start begins the export process. The process is organized as a pipeline of
// goroutines that run concurrently. This function returns as soon as the
// process has been started, and the caller can watch the Done() channel to be
// informed when the process completes.
func (be *BlockExporter) Start() error {
	var blockFileNo, txFileNo, txInFileNo, txOutFileNo uint32

	// This is defined so if any of the writers fail to open, we can close the
	// rest before returning the error.
	closeWriters := func(writers ...*RotatingWriter) {
		for _, writer := range writers {
			err := writer.Close()
			if err != nil {
				// TODO: Log the error
			}
		}
	}

	blocksOutput, err := be.cfg.OpenWriter("blocks-%d.csv.gz", &blockFileNo)
	if err != nil {
		return err
	}
	txsOutput, err := be.cfg.OpenWriter("txs-%d.csv.gz", &txFileNo)
	if err != nil {
		closeWriters(blocksOutput)
		return err
	}
	txInsOutput, err := be.cfg.OpenWriter("txins-%d.csv.gz", &txInFileNo)
	if err != nil {
		closeWriters(blocksOutput, txsOutput)
		return err
	}
	txOutsOutput, err := be.cfg.OpenWriter("txouts-%d.csv.gz", &txOutFileNo)
	if err != nil {
		closeWriters(blocksOutput, txsOutput, txInsOutput)
		return err
	}

	var (
		blockWriter = newRecordWriter(blocksOutput, be.cfg.FileSizeLimit)
		txWriter    = newRecordWriter(txsOutput, be.cfg.FileSizeLimit)
		txInWriter  = newRecordWriter(txInsOutput, be.cfg.FileSizeLimit)
		txOutWriter = newRecordWriter(txOutsOutput, be.cfg.FileSizeLimit)
	)

	// Create communication channels.
	blockDescs := make(chan *blockDescriptor, 1024)
	var (
		blockRecords = make(chan []string, 32)
		txRecords    = make(chan []string, 256)
		txInRecords  = make(chan []string, 256)
		txOutRecords = make(chan []string, 256)
	)

	// Start up the read and process handling goroutines.  This setup allows
	// blocks to be read from disk in parallel while being processed.
	var wg1, wg2 sync.WaitGroup

	wg1.Add(1)
	go be.loadBlockHashes(&wg1, blockDescs)

	for i := 0; i < 4; i++ {
		wg1.Add(1)
		go be.processBlocks(&wg1, blockDescs,
			blockRecords, txRecords, txInRecords, txOutRecords)
	}

	wg2.Add(4)
	go be.writeRecords(&wg2, blockWriter, blockRecords)
	go be.writeRecords(&wg2, txWriter, txRecords)
	go be.writeRecords(&wg2, txInWriter, txInRecords)
	go be.writeRecords(&wg2, txOutWriter, txOutRecords)

	// Wait for the import to finish in a separate goroutine and signal
	// the status handler when done.
	go func() {
		// Wait for all blocks to be processed.
		wg1.Wait()

		// Shut down the file writer goroutines and wait for them to exit.
		close(blockRecords)
		close(txRecords)
		close(txInRecords)
		close(txOutRecords)
		wg2.Wait()

		// Close output files.
		if err := blockWriter.Close(); err != nil {
			be.errChan <- err
		}
		if err := txWriter.Close(); err != nil {
			be.errChan <- err
		}
		if err := txInWriter.Close(); err != nil {
			be.errChan <- err
		}
		if err := txOutWriter.Close(); err != nil {
			be.errChan <- err
		}

		// Now signal the export process as done.
		close(be.errChan)
		close(be.doneChan)
	}()

	return nil
}

// Stop signals to all goroutines that are part of the export process to end
// immediately. This function may return before they all exit.
func (be *BlockExporter) Stop() {
	select {
	case <-be.quit:
		// Exporter is already stopping.
	default:
		close(be.quit)
	}
}

// Errors returns a channel with errors that occur during the export process.
// The channel is closed when the process completes or ends prematurely.
func (be *BlockExporter) Errors() <-chan error {
	return be.errChan
}

// Done returns a channel that is closed when the process completes or ends
// prematurely.
func (be *BlockExporter) Done() <-chan struct{} {
	return be.doneChan
}

// TotalBlocks returns the total number of blocks that are to be processed.
func (be *BlockExporter) TotalBlocks() uint {
	return be.cfg.EndHeight - be.cfg.StartHeight + 1
}

// BlocksProcessed returns the number of blocks processed so far.
func (be *BlockExporter) BlocksProcessed() uint {
	return uint(atomic.LoadUint32(&be.numProcessed))
}

// loadHashes fetches the hashes of all blocks in the export range and sends
// them to the blockDescs parameter channel.
//
// This function is intended to be run as a goroutine.
func (be *BlockExporter) loadBlockHashes(wg *sync.WaitGroup,
	blockDescs chan<- *blockDescriptor) {

	defer wg.Done()

	for height := be.cfg.StartHeight; height <= be.cfg.EndHeight; height++ {
		hash, err := be.chain.BlockHashByHeight(int32(height))
		if err != nil {
			be.errChan <- err
			break
		}

		blockDesc := &blockDescriptor{
			hash:   hash,
			height: int32(height),
		}
		select {
		case blockDescs <- blockDesc:
		case <-be.quit:
			return
		}
	}
	close(blockDescs)
}

// processBlocks receives block hashes from the blockDescs parameter channel,
// uses processBlock to fetch and encode block data, and sends the resulting
// records to the output channels.
//
// This function is intended to be run as a goroutine.
func (be *BlockExporter) processBlocks(wg *sync.WaitGroup,
	blockDescs <-chan *blockDescriptor,
	blockRecords, txRecords, txInRecords, txOutRecords chan<- []string) {

	defer wg.Done()

	for {
		select {
		case blockDesc, more := <-blockDescs:
			if !more {
				return
			}
			err := be.processBlock(blockDesc,
				blockRecords, txRecords, txInRecords, txOutRecords)
			if err != nil {
				be.errChan <- err
			}
			atomic.AddUint32(&be.numProcessed, 1)

		case <-be.quit:
			return
		}
	}
}

// writeRecords receives records from the records parameter channel and writes
// them to an output destination.
//
// This function is intended to be run as a goroutine.
func (be *BlockExporter) writeRecords(wg *sync.WaitGroup, writer *recordWriter,
	records <-chan []string) {

	defer wg.Done()

	for {
		record, more := <-records
		if !more {
			return
		}

		// Do not write if shutting down.
		select {
		case <-be.quit:
			continue
		default:
		}

		err := writer.Write(record)
		if err != nil {
			be.errChan <- err
		}
	}
}

// processBlock processes a block descriptor by fetching the block from the
// database, encoding the block data into records, and sending the resulting
// records to the output channels.
func (be *BlockExporter) processBlock(blockDesc *blockDescriptor,
	blockRecords, txRecords, txInRecords, txOutRecords chan<- []string) error {

	var blockBytes []byte
	err := be.db.View(func(dbTx database.Tx) error {
		var err error
		blockBytes, err = dbTx.FetchBlock(blockDesc.hash)
		return err
	})
	if err != nil {
		return err
	}

	// Create the encapsulated block and set the height appropriately.
	block, err := btcutil.NewBlockFromBytes(blockBytes)
	if err != nil {
		return err
	}
	block.SetHeight(blockDesc.height)

	blockRecord, err := be.encoder.GenBlockRecord(block)
	if err != nil {
		return err
	}
	blockRecords <- blockRecord

	for _, tx := range block.Transactions() {
		txRecord, err := be.encoder.GenTxRecord(tx, uint(block.Height()))
		if err != nil {
			return err
		}
		txRecords <- txRecord

		// Skip BIP 30 duplicate transactions.
		if (block.Height() == 91842 && tx.Hash().IsEqual(block91842TxHash)) ||
			(block.Height() == 91880 && tx.Hash().IsEqual(block91880TxHash)) {
			continue
		}

		for i, txIn := range tx.MsgTx().TxIn {
			txInRecord, err := be.encoder.GenTxInRecord(
				tx.Hash(), i, txIn, tx.Index() == 0)
			if err != nil {
				return err
			}
			txInRecords <- txInRecord
		}

		for i, txOut := range tx.MsgTx().TxOut {
			txOutRecord, err := be.encoder.GenTxOutRecord(tx.Hash(), i, txOut)
			if err != nil {
				return err
			}
			txOutRecords <- txOutRecord
		}
	}

	return nil
}

// newHashFromStr converts the passed big-endian hex string into a wire.Hash. It
// only differs from the one available in chainhash in that it panics on an
// error since it should only be called with hard-coded, and therefore known
// good, hashes.
func newHashFromStr(hexStr string) *chainhash.Hash {
	hash, err := chainhash.NewHashFromStr(hexStr)
	if err != nil {
		panic(err)
	}
	return hash
}
