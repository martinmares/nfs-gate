function Header(header)
  if header.identifier == "technicke-detaily" or header.identifier == "5-vice-fyzickych-clusteru" or header.identifier == "provozni-omezeni" then
    return {pandoc.RawBlock("typst", "#pagebreak()"), header}
  end
end
