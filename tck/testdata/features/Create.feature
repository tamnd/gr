Feature: Create

  Scenario: Create a single node
    Given an empty graph
    When executing query:
      """
      CREATE (:N {x: 1}) RETURN 1 AS r
      """
    Then the result should be:
      | r |
      | 1 |

  Scenario: Create multiple nodes
    Given an empty graph
    When executing query:
      """
      CREATE (:A), (:B) RETURN 1 AS r
      """
    Then the result should be:
      | r |
      | 1 |

  Scenario: Create a relationship
    Given an empty graph
    When executing query:
      """
      CREATE (a:P)-[:KNOWS]->(b:P) RETURN 1 AS r
      """
    Then the result should be:
      | r |
      | 1 |
