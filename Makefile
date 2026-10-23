NAME := psbin
MODULE := $(shell go list -m)

run:
	go run .

prod:
	go build -trimpath -ldflags="-s -w" -o $(NAME) .
	@$(MAKE) install

install:
	mkdir -p ~/bin
	rm -f ~/bin/$(NAME)
	mv ./$(NAME) ~/bin/$(NAME)

# direct imports of each package (own packages shown by name)
deps:
	@go list -f '{{.Name}}: {{join .Imports " "}}' ./... | sed 's#$(MODULE)/internal/##g'

.PHONY: run prod install deps
