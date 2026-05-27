#!/bin/bash

letters=(B C E H J K L M Q T W X Y Z)

for fletter in "${letters[@]}"; do
  for mletter in "${letters[@]}"; do
    if [ $fletter != $mletter ]; then
      for lletter in "${letters[@]}"; do
        if [ $fletter != $lletter ]; then
          if [ $mletter != $lletter ]; then
            echo "[ ] ${fletter}${mletter}${lletter}" >> temp.txt
          fi
        fi
      done
    fi 
  done
done

sort -R temp.txt > letters.txt
rm temp.txt