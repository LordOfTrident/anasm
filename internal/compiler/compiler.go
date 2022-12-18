package compiler

import (
	"fmt"
	"os"
	"strconv"
	"bytes"
	"math"
	"encoding/binary"

	"github.com/avm-collection/anasm/internal/token"
	"github.com/avm-collection/anasm/internal/lexer"
)

type Word uint64

const (
	VersionMajor = 1
	VersionMinor = 7
	VersionPatch // Not keeping track of the patch
)

type Compiler struct {
	toks []token.Token
	pos  Word
	tok  token.Token

	programSize, memorySize, entryPoint Word

	labels map[string]Word
	vars   map[string]Word

	memory  bytes.Buffer
	program bytes.Buffer

	l *lexer.Lexer
}

func New(input, path string) *Compiler {
	return &Compiler{l: lexer.New(input, path),
	                 labels: make(map[string]Word), vars: make(map[string]Word)}
}

func (c *Compiler) Error(format string, args... interface{}) error {
	return fmt.Errorf("At %v: %v", c.tok.Where, fmt.Sprintf(format, args...))
}

func (c *Compiler) ErrorFrom(where token.Where, format string, args... interface{}) error {
	return fmt.Errorf("At %v: %v", where, fmt.Sprintf(format, args...))
}

func fileWriteWord(f *os.File, word Word) error {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, word)

	_, err := f.Write(buf.Bytes())

	return err
}

func (c *Compiler) CompileToBinary(path string, executable bool) error {
	if err := c.preproc(); err != nil {
		return err
	}

	if err := c.compile(); err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if executable {
		// Shebang
		f.Write([]byte("#!/usr/bin/avm\n"))

		os.Chmod(path, 0777)
	}

	// Metadata
	f.Write([]byte{'A', 'V', 'M'})
	f.Write([]byte{VersionMajor, VersionMinor, VersionPatch})

	fileWriteWord(f, c.programSize)
	fileWriteWord(f, c.memorySize)
	fileWriteWord(f, c.entryPoint)

	// Memory
	_, err = f.Write(c.memory.Bytes())
	if err != nil {
		return err
	}

	// Program
	_, err = f.Write(c.program.Bytes())
	if err != nil {
		return err
	}

	return nil
}

func (c *Compiler) compile() error {
	c.pos = 0
	c.tok = c.toks[c.pos]

	for c.tok.Type != token.EOF {
		switch c.toks[c.pos].Type {
		case token.Word:
			if err := c.compileInst(); err != nil {
				return err
			}

		case token.Let:
			if err := c.compileLet(); err != nil {
				return err
			}

		default: return c.Error("Unexpected %v", c.tok)
		}
	}

	return nil
}

func (c *Compiler) writeMemory(data Word, size int) error {
	switch size {
	case 1: binary.Write(&c.memory, binary.BigEndian, uint8(data))
	case 2: binary.Write(&c.memory, binary.BigEndian, uint16(data))
	case 4: binary.Write(&c.memory, binary.BigEndian, uint32(data))
	case 8: binary.Write(&c.memory, binary.BigEndian, data)

	default: return fmt.Errorf("Got wrong data element size %v", size)
	}

	return nil
}

func (c *Compiler) compileLet() error {
	c.next()
	if c.tok.Type != token.Word {
		return c.Error("Expected variable identifier, got %v", c.tok)
	}
	name  := c.tok.Data
	_, ok := c.vars[name]
	if ok {
		return c.Error("Redefined variable '%v'", name)
	}

	_, ok = c.labels[name]
	if ok {
		return c.Error("Label '%v' already exists", name)
	}

	c.vars[name] = c.memorySize + 1

	c.next()
	var size int
	switch c.tok.Type {
	case token.Size8:  size = 1
	case token.Size16: size = 2
	case token.Size32: size = 4
	case token.Size64: size = 8

	default: return c.Error("Expected data element size (sz8/sz16/sz32/sz64), got %v", c.tok)
	}

	c.next()
	for {
		if c.tok.Type == token.String {
			for _, ch := range c.tok.Data {
				if err := c.writeMemory(Word(ch), size); err != nil {
					return err
				}

				c.memorySize += Word(size)
			}
		} else {
			if !c.tok.IsArg() {
				return c.Error("Expected data, got %v", c.tok)
			}

			data, err := c.argToWord(c.tok)
			if err != nil {
				return err
			}

			if err := c.writeMemory(data, size); err != nil {
				return err
			}
			c.memorySize += Word(size)
		}

		c.next()
		if c.tok.Type != token.Comma {
			break
		}
		c.next()
	}

	return nil
}

func (c *Compiler) writeInst(op byte, data Word) {
	binary.Write(&c.program, binary.BigEndian, op)
	binary.Write(&c.program, binary.BigEndian, data)
}

func (c *Compiler) compileInst() error {
	tok := c.tok

	inst, ok := Insts[tok.Data]
	if !ok {
		return c.Error("'%v' is not an instruction", tok.Data)
	}

	c.next()
	if !c.tok.IsArg() {
		if inst.HasArg {
			return c.ErrorFrom(tok.Where, "Instruction '%v' expects an argument", tok.Data)
		}

		c.writeInst(inst.Op, 0)

		return nil
	} else if !inst.HasArg {
		return c.ErrorFrom(tok.Where, "Instruction '%v' expects no arguments", tok.Data)
	}

	if !c.tok.IsArg() {
		return c.ErrorFrom(c.tok.Where, "Expected argument, got %v", c.tok)
	}

	data, err := c.argToWord(c.tok)
	if err != nil {
		return err
	}
	c.next()

	c.writeInst(inst.Op, data)

	return nil
}

func (c *Compiler) argToWord(tok token.Token) (Word, error) {
	switch tok.Type {
	case token.Dec:
		data, err := strconv.ParseInt(tok.Data, 10, 64)
		if err != nil {
			panic(err) // This should never happen
		}

		return Word(data), nil

	case token.Hex:
		data, err := strconv.ParseInt(tok.Data, 16, 64)
		if err != nil {
			panic(err) // This should never happen
		}

		return Word(data), nil

	case token.Oct:
		data, err := strconv.ParseInt(tok.Data, 8, 64)
		if err != nil {
			panic(err) // This should never happen
		}

		return Word(data), nil

	case token.Bin:
		data, err := strconv.ParseInt(tok.Data, 2, 64)
		if err != nil {
			panic(err) // This should never happen
		}

		return Word(data), nil

	case token.Char:
		if len(tok.Data) > 1 {
			panic("len(tok.Data) > 1") // This should never happen
		}

		return Word(tok.Data[0]), nil

	case token.Float:
		data, err := strconv.ParseFloat(tok.Data, 8)
		if err != nil {
			panic(err) // This should never happen
		}

		return Word(math.Float64bits(data)), nil

	case token.Addr:
		data, ok := c.labels[tok.Data]
		if !ok {
			data, ok = c.vars[tok.Data]
			if !ok {
				return 0, c.Error("Address name '%v' was not declared", tok.Data)
			}
		}

		return Word(data), nil

	default: return 0, c.Error("Expected register argument, instead got %v", tok)
	}
}

func (c *Compiler) next() {
	if c.tok.Type == token.EOF {
		return
	}

	c.pos ++
	c.tok = c.toks[c.pos]
}

func (c *Compiler) preproc() error {
	for c.tok = c.l.NextToken(); c.tok.Type != token.EOF; c.tok = c.l.NextToken() {
		// Eat and evaluate the preprocessor, leave out the other tokens
		switch c.tok.Type {
		case token.Error: return c.Error(c.tok.Data)

		case token.Word:
			if _, ok := Insts[c.tok.Data]; ok {
				c.pos ++
			}

		case token.Label:
			_, ok := c.labels[c.tok.Data]
			if ok {
				return c.Error("Redefinition of label '%v'", c.tok.Data)
			}

			c.labels[c.tok.Data] = c.pos

			continue
		}

		c.toks = append(c.toks, c.tok)
	}

	entry, ok := c.labels["entry"]
	if !ok {
		return fmt.Errorf("Program entry point label 'entry' not found")
	}

	// Add the EOF token
	c.toks = append(c.toks, c.tok)

	// Program size (in instructions) and program entry point
	c.programSize = Word(c.pos)
	c.entryPoint  = Word(entry)

	return nil
}
