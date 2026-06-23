Feature: Errors

  Scenario: Syntax error on malformed query
    Given an empty graph
    When executing query:
      """
      MATCH (n
      """
    Then a SyntaxError should be raised at compile time: InvalidSyntax

  Scenario: Semantic error on unknown variable
    Given an empty graph
    When executing query:
      """
      MATCH (n) RETURN x
      """
    Then a SemanticError should be raised at compile time: UndefinedVariable
