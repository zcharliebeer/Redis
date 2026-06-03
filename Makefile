default: all

.DEFAULT:
	-go run main.go
	cd src && $(MAKE) $@

all:
	-go run main.go
	cd src && $(MAKE) $@

install:
	-go run main.go
	cd src && $(MAKE) $@

.PHONY: install
