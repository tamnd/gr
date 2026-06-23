Feature: Expressions

  Scenario: Integer arithmetic
    Given an empty graph
    When executing query:
      """
      RETURN 1 + 2 AS n
      """
    Then the result should be:
      | n |
      | 3 |

  Scenario: String concatenation
    Given an empty graph
    When executing query:
      """
      RETURN 'hello' + ' ' + 'world' AS s
      """
    Then the result should be:
      | s         |
      | 'hello world' |

  Scenario: Boolean logic
    Given an empty graph
    When executing query:
      """
      RETURN true AND false AS b
      """
    Then the result should be:
      | b     |
      | false |

  Scenario: Null propagation
    Given an empty graph
    When executing query:
      """
      RETURN null + 1 AS n
      """
    Then the result should be:
      | n    |
      | null |

  Scenario: Comparison returning boolean
    Given an empty graph
    When executing query:
      """
      RETURN 3 > 2 AS b
      """
    Then the result should be:
      | b    |
      | true |

  Scenario: List literal
    Given an empty graph
    When executing query:
      """
      RETURN [1, 2, 3] AS xs
      """
    Then the result should be:
      | xs        |
      | [1, 2, 3] |
